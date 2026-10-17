// pool.cpp — Pool mode event loop implementation.
//
// Reads line-delimited JSON commands from stdin, dispatches to
// pool_load / pool_render (defined per-format in formats/<fmt>.cpp),
// writes JSON responses to stdout.
//
// Protocol:
//   → {"cmd":"load","input":"/path","license_path":"/path"}
//   ← {"status":"ok","page_count":N}
//
//   → {"cmd":"render","page_start":1,"page_end":25,"output":"/tmp/chunk.pdf"}
//   ← {"status":"ok","output":"/tmp/chunk.pdf"}
//
//   → {"cmd":"quit"}
//   ← (process exits with code 0)
//
// Error response:
//   ← {"status":"error","code":N,"detail":"..."}

#include "pool.h"

#include <fcntl.h>
#include <poll.h>
#include <sys/socket.h>
#include <sys/wait.h>
#include <unistd.h>

#include <algorithm>
#include <atomic>
#include <cerrno>
#include <chrono>
#include <csignal>
#include <cstdlib>
#include <cstring>
#include <deque>
#include <fstream>
#include <iostream>
#include <sstream>
#include <string>
#include <thread>
#include <vector>

#include "error.h"
#include "license.h"

namespace office_convert {

namespace {

// Set by leader before fork (=0) and by each forked child to its 1..N-1 index.
// Read by Heartbeat::emit() and exposed via current_pool_index() so per-format
// load-progress callbacks can tag their output on the shared stderr pipe.
int g_pool_index = 0;

// Tier-1 observability: stream a per-second pulse to stderr while a long-running
// command is in flight, so the Python orchestrator can distinguish "still
// working" from "deadlocked" during the 600s pool_load_timeout window. Aspose's
// load/render APIs are otherwise fully opaque from outside the process. See
// aidlc-docs/audit.md 2026-05-14 stress_test_100mb.docx investigation.

// Read VmRSS and VmSwap from /proc/self/status in a single open. Swap is
// load-bearing on this service — the worker sits behind memswap_limit=12g
// and Aspose loads on large inputs are expected to page out. Reporting RSS
// alone would hide the most important failure mode: RSS plateaus at the
// cgroup limit while VmSwap silently grows, eventually OOM-killing the
// container even though RSS looks "stable".
void read_vm_mem(long& rss_bytes, long& swap_bytes) {
    rss_bytes = -1;
    swap_bytes = -1;
    std::ifstream f("/proc/self/status");
    std::string line;
    while (std::getline(f, line)) {
        if (rss_bytes < 0 && line.rfind("VmRSS:", 0) == 0) {
            std::istringstream ss(line.substr(6));
            long kb = -1;
            if (ss >> kb) rss_bytes = kb * 1024L;
        } else if (swap_bytes < 0 && line.rfind("VmSwap:", 0) == 0) {
            std::istringstream ss(line.substr(7));
            long kb = -1;
            if (ss >> kb) swap_bytes = kb * 1024L;
        }
        if (rss_bytes >= 0 && swap_bytes >= 0) break;
    }
}

long read_cpu_jiffies() {
    // /proc/self/stat: after the parenthesised comm field, utime is field 14
    // and stime is field 15 (1-indexed). The comm field can contain arbitrary
    // bytes including spaces and parens, so split at the LAST ')' first.
    std::ifstream f("/proc/self/stat");
    std::string content;
    if (!std::getline(f, content)) return -1;
    auto rp = content.rfind(')');
    if (rp == std::string::npos) return -1;
    std::istringstream ss(content.substr(rp + 1));
    std::string tok;
    long utime = 0, stime = 0;
    for (int i = 0; i < 13; ++i) {
        if (!(ss >> tok)) return -1;
        if (i == 11) { try { utime = std::stol(tok); } catch (...) { return -1; } }
        if (i == 12) { try { stime = std::stol(tok); } catch (...) { return -1; } }
    }
    return utime + stime;
}

class Heartbeat {
public:
    explicit Heartbeat(const char* phase)
        : phase_(phase),
          interval_ms_(env_interval_ms()),
          running_(true),
          start_(std::chrono::steady_clock::now()) {
        if (interval_ms_ <= 0) return;  // disabled
        thread_ = std::thread([this] { loop(); });
    }

    ~Heartbeat() {
        if (interval_ms_ <= 0) return;
        running_.store(false, std::memory_order_release);
        if (thread_.joinable()) thread_.join();
    }

    Heartbeat(const Heartbeat&) = delete;
    Heartbeat& operator=(const Heartbeat&) = delete;

private:
    static int env_interval_ms() {
        if (const char* s = std::getenv("OFFICE_CONVERT_HEARTBEAT_MS")) {
            try { return std::stoi(s); } catch (...) { /* fall through */ }
        }
        return 2000;  // default: 2s
    }

    void emit() {
        long rss = -1, swap = -1;
        read_vm_mem(rss, swap);
        long jiffies = read_cpu_jiffies();
        long elapsed_s = std::chrono::duration_cast<std::chrono::seconds>(
            std::chrono::steady_clock::now() - start_).count();
        std::ostringstream oss;
        oss << "{\"type\":\"heartbeat\",\"pool_index\":" << g_pool_index
            << ",\"phase\":\"" << phase_
            << "\",\"elapsed_s\":" << elapsed_s
            << ",\"rss_bytes\":" << rss
            << ",\"swap_bytes\":" << swap
            << ",\"cpu_jiffies\":" << jiffies << "}\n";
        // Single write of a single line — minimises interleaving against any
        // stderr Aspose may emit internally.
        std::cerr << oss.str() << std::flush;
    }

    void loop() {
        emit();  // immediate first pulse so a sub-interval death is still visible
        const auto slice = std::chrono::milliseconds(100);
        const int slices_per_interval = std::max(1, interval_ms_ / 100);
        while (running_.load(std::memory_order_acquire)) {
            for (int i = 0; i < slices_per_interval; ++i) {
                if (!running_.load(std::memory_order_acquire)) return;
                std::this_thread::sleep_for(slice);
            }
            if (running_.load(std::memory_order_acquire)) emit();
        }
    }

    const char* phase_;
    int interval_ms_;
    std::atomic<bool> running_;
    std::chrono::steady_clock::time_point start_;
    std::thread thread_;
};

// Minimal JSON value extraction (avoids pulling in a JSON library).
// Finds "key": "value" or "key":"value" in a JSON string.
std::string json_string(const std::string& json, const std::string& key) {
    // Look for "key" followed by : and then "value"
    std::string needle = "\"" + key + "\"";
    auto pos = json.find(needle);
    if (pos == std::string::npos) return "";
    pos += needle.size();
    // Skip whitespace and colon
    while (pos < json.size() && (json[pos] == ' ' || json[pos] == '\t' || json[pos] == ':')) ++pos;
    // Expect opening quote
    if (pos >= json.size() || json[pos] != '"') return "";
    ++pos;  // skip the quote
    auto end = json.find('"', pos);
    if (end == std::string::npos) return "";
    return json.substr(pos, end - pos);
}

int json_int(const std::string& json, const std::string& key) {
    std::string needle = "\"" + key + "\"";
    auto pos = json.find(needle);
    if (pos == std::string::npos) return -1;
    pos += needle.size();
    // Skip whitespace and colon
    while (pos < json.size() && (json[pos] == ' ' || json[pos] == '\t' || json[pos] == ':')) ++pos;
    try {
        return std::stoi(json.substr(pos));
    } catch (...) {
        return -1;
    }
}

void respond_ok(int page_count) {
    std::cout << "{\"status\":\"ok\",\"page_count\":" << page_count << "}" << std::endl;
}

void respond_ok_render(const std::string& output) {
    std::cout << "{\"status\":\"ok\",\"output\":\"" << output << "\"}" << std::endl;
}

void respond_error(int code, const std::string& detail) {
    // Escape quotes in detail
    std::string escaped;
    for (char c : detail) {
        if (c == '"') escaped += "\\\"";
        else if (c == '\\') escaped += "\\\\";
        else if (c == '\n') escaped += "\\n";
        else escaped += c;
    }
    std::cout << "{\"status\":\"error\",\"code\":" << code
              << ",\"detail\":\"" << escaped << "\"}" << std::endl;
}

// ============================================================================
// fork-after-load: leader/child machinery
//
// One leader process loads the document. After load, it forks pool_size-1
// children that share the loaded Document via copy-on-write. The leader
// multiplexes its stdin and N socketpair fds with poll(), dispatching seq-
// tagged render commands to whichever child is free (or rendering itself).
//
// Risk: fork() inside a process that has loaded a heavily-multithreaded native
// SDK (Aspose's CodePorting framework spawns its own threads during library
// init / document load) may leave the children with held locks owned by
// non-existent threads. The feature is gated behind a Python-side setting so
// it can be disabled if it misbehaves on a given input.
// ============================================================================

std::string escape_json(const std::string& s) {
    std::string out;
    out.reserve(s.size());
    for (char c : s) {
        if (c == '"') out += "\\\"";
        else if (c == '\\') out += "\\\\";
        else if (c == '\n') out += "\\n";
        else out += c;
    }
    return out;
}

// Write a JSON line to a file descriptor. Lines stay atomic up to PIPE_BUF.
bool fd_write_line(int fd, const std::string& line) {
    std::string buf = line;
    if (buf.empty() || buf.back() != '\n') buf += '\n';
    const char* p = buf.data();
    size_t left = buf.size();
    while (left > 0) {
        ssize_t n = ::write(fd, p, left);
        if (n < 0) {
            if (errno == EINTR) continue;
            return false;
        }
        if (n == 0) return false;
        p += n;
        left -= static_cast<size_t>(n);
    }
    return true;
}

// Per-fd accumulator: read whatever's available, return any complete lines.
struct LineAccumulator {
    std::string buf;
    // Append the available bytes and extract any newline-terminated lines.
    // Returns false on EOF or fatal read error.
    bool drain(int fd, std::vector<std::string>& out) {
        char tmp[4096];
        while (true) {
            ssize_t n = ::read(fd, tmp, sizeof(tmp));
            if (n > 0) {
                buf.append(tmp, static_cast<size_t>(n));
                size_t pos;
                while ((pos = buf.find('\n')) != std::string::npos) {
                    out.emplace_back(buf.substr(0, pos));
                    buf.erase(0, pos + 1);
                }
                // Continue reading if there might be more
                if (n < static_cast<ssize_t>(sizeof(tmp))) return true;
            } else if (n == 0) {
                return false;  // EOF
            } else {
                if (errno == EINTR) continue;
                if (errno == EAGAIN || errno == EWOULDBLOCK) return true;
                return false;
            }
        }
    }
};

// Build a response JSON string carrying the original seq.
std::string make_response_render(int seq, const std::string& output) {
    std::ostringstream oss;
    oss << "{\"seq\":" << seq << ",\"status\":\"ok\",\"output\":\""
        << escape_json(output) << "\"}";
    return oss.str();
}

std::string make_response_error(int seq, int code, const std::string& detail) {
    std::ostringstream oss;
    oss << "{\"seq\":" << seq << ",\"status\":\"error\",\"code\":" << code
        << ",\"detail\":\"" << escape_json(detail) << "\"}";
    return oss.str();
}

// Child process: read render commands from sock_fd, execute via pool_render,
// reply on the same fd. Quit cleanly on EOF or {"cmd":"quit"}.
int child_render_loop(int sock_fd) {
    LineAccumulator acc;
    while (true) {
        std::vector<std::string> lines;
        if (!acc.drain(sock_fd, lines)) {
            return EXIT_OK;  // EOF or error — leader gone, exit quietly
        }
        for (const auto& line : lines) {
            if (line.empty()) continue;
            const std::string cmd = json_string(line, "cmd");
            if (cmd == "quit") return EXIT_OK;
            if (cmd != "render") continue;
            int seq = json_int(line, "seq");
            int ps = json_int(line, "page_start");
            int pe = json_int(line, "page_end");
            std::string out = json_string(line, "output");
            if (ps < 1 || pe < ps || out.empty()) {
                fd_write_line(sock_fd, make_response_error(seq, EXIT_RENDER_FAILURE, "bad render args"));
                continue;
            }
            try {
                Heartbeat hb("render");
                pool_render(ps, pe, out);
                fd_write_line(sock_fd, make_response_render(seq, out));
            } catch (const WorkerError& e) {
                fd_write_line(sock_fd, make_response_error(seq, e.exit_code(), e.what()));
            } catch (const std::bad_alloc&) {
                fd_write_line(sock_fd, make_response_error(seq, EXIT_OOM, "render: bad_alloc"));
                return EXIT_OOM;  // OOM is fatal for the child
            } catch (const std::exception& e) {
                fd_write_line(sock_fd, make_response_error(seq, EXIT_RENDER_FAILURE,
                    std::string("render: ") + e.what()));
            } catch (...) {
                // Aspose::Cells::CellsException does not derive from std::exception.
                fd_write_line(sock_fd, make_response_error(seq, EXIT_RENDER_FAILURE,
                    "render: non-standard exception"));
            }
        }
    }
}

struct ChildSlot {
    pid_t pid = -1;
    int sock_fd = -1;
    bool busy = false;
    bool dead = false;      // exited; never dispatched to or polled again
    int in_flight_seq = -1; // seq of the command it is rendering, if busy
    LineAccumulator acc;
};

}  // namespace

int pool_loop_forked(const std::string& format, const std::string& license_path, int pool_size) {
    (void)format;  // format is enforced structurally by the binary identity

    // A write to a dead child's socket must fail with EPIPE, not kill the
    // leader (and every in-flight chunk with it). Children inherit this.
    ::signal(SIGPIPE, SIG_IGN);

    // License first (in leader, before fork — children inherit licensed state).
    try {
        apply_license(license_path);
    } catch (const WorkerError& e) {
        respond_error(e.exit_code(), e.what());
        return e.exit_code();
    } catch (const std::exception& e) {
        respond_error(EXIT_LICENSE_INVALID, e.what());
        return EXIT_LICENSE_INVALID;
    }

    // Wait for the load command before forking — we need the document loaded
    // FIRST so children inherit it via COW. Anything else from stdin before
    // load is an error.
    std::string line;
    if (!std::getline(std::cin, line)) return EXIT_OK;
    if (json_string(line, "cmd") != "load") {
        respond_error(EXIT_RENDER_FAILURE, "first command must be 'load' in fork pool mode");
        return EXIT_RENDER_FAILURE;
    }
    const std::string input = json_string(line, "input");
    if (input.empty()) {
        respond_error(EXIT_RENDER_FAILURE, "missing input path");
        return EXIT_RENDER_FAILURE;
    }
    int page_count = 0;
    try {
        Heartbeat hb("load");
        page_count = pool_load(input);
    } catch (const WorkerError& e) {
        respond_error(e.exit_code(), e.what());
        return e.exit_code();
    } catch (const std::bad_alloc&) {
        respond_error(EXIT_OOM, "load: bad_alloc");
        return EXIT_OOM;
    } catch (const std::exception& e) {
        respond_error(EXIT_RENDER_FAILURE, std::string("load: ") + e.what());
        return EXIT_RENDER_FAILURE;
    }

    // Fork children. Leader is index 0; children get 1..N-1.
    std::vector<ChildSlot> children;
    children.reserve(pool_size - 1);
    for (int i = 1; i < pool_size; ++i) {
        int socks[2];
        if (::socketpair(AF_UNIX, SOCK_STREAM, 0, socks) < 0) {
            respond_error(EXIT_RENDER_FAILURE, std::string("socketpair: ") + std::strerror(errno));
            return EXIT_RENDER_FAILURE;
        }
        pid_t pid = ::fork();
        if (pid < 0) {
            ::close(socks[0]); ::close(socks[1]);
            respond_error(EXIT_RENDER_FAILURE, std::string("fork: ") + std::strerror(errno));
            return EXIT_RENDER_FAILURE;
        }
        if (pid == 0) {
            // CHILD: close leader's end of every prior socketpair, close stdin/stdout
            ::close(socks[0]);
            for (auto& c : children) ::close(c.sock_fd);
            // Point stdin/stdout at /dev/null rather than closing them, so a
            // later open() inside Aspose can't be handed fd 0/1 and have stray
            // stdout writes land inside a chunk PDF.
            if (int devnull = ::open("/dev/null", O_RDWR); devnull >= 0) {
                ::dup2(devnull, STDIN_FILENO);
                ::dup2(devnull, STDOUT_FILENO);
                if (devnull > STDERR_FILENO) ::close(devnull);
            }
            // stderr stays open so heartbeats reach the orchestrator.
            g_pool_index = i;
            // Never unwind out of a forked copy of a multithreaded process:
            // atexit handlers and static destructors must not run here.
            int rc = EXIT_RENDER_FAILURE;
            try {
                rc = child_render_loop(socks[1]);
            } catch (...) {
            }
            _exit(rc);
        }
        // PARENT
        ::close(socks[1]);
        ChildSlot slot;
        slot.pid = pid;
        slot.sock_fd = socks[0];
        slot.busy = false;
        children.push_back(std::move(slot));
    }

    // Acknowledge load to the orchestrator now that children are up.
    // Include seq=0 so the Python seq demuxer matches it correctly.
    std::cout << "{\"seq\":0,\"status\":\"ok\",\"page_count\":" << page_count << "}" << std::endl;

    // Pending render commands queue when all slots (leader + children) are busy.
    std::deque<std::string> pending;
    bool leader_busy = false;
    bool stdin_open = true;
    bool quitting = false;

    // A child that exits is retired: its in-flight command gets an error reply
    // (otherwise the orchestrator waits out the full chunk timeout), and it is
    // closed, reaped and never dispatched to again.
    auto retire_child = [&](ChildSlot& c) {
        if (c.busy && c.in_flight_seq >= 0) {
            std::cout << make_response_error(c.in_flight_seq, EXIT_RENDER_FAILURE,
                "forked render child exited") << std::endl;
        }
        if (c.sock_fd >= 0) ::close(c.sock_fd);
        if (c.pid > 0) {
            int status = 0;
            ::waitpid(c.pid, &status, 0);
        }
        c.sock_fd = -1;
        c.pid = -1;
        c.busy = false;
        c.in_flight_seq = -1;
        c.dead = true;
    };

    auto dispatch_to_child = [&](ChildSlot& c, const std::string& cmd_line) -> bool {
        c.busy = true;
        c.in_flight_seq = json_int(cmd_line, "seq");
        if (fd_write_line(c.sock_fd, cmd_line)) return true;
        // Child already gone (EPIPE). Retire it without replying: the caller
        // keeps the command and hands it to another slot.
        c.busy = false;
        c.in_flight_seq = -1;
        retire_child(c);
        return false;
    };

    auto try_assign = [&](const std::string& cmd_line) -> bool {
        for (auto& c : children) {
            if (!c.dead && !c.busy && dispatch_to_child(c, cmd_line)) return true;
        }
        if (!leader_busy) {
            // Leader renders inline. This blocks the poll loop briefly, but
            // we accept that because the alternative (a fourth child) duplicates
            // the COW page set in vain — the leader is already paying for the
            // document either way.
            leader_busy = true;
            int seq = json_int(cmd_line, "seq");
            int ps = json_int(cmd_line, "page_start");
            int pe = json_int(cmd_line, "page_end");
            std::string out = json_string(cmd_line, "output");
            if (ps < 1 || pe < ps || out.empty()) {
                std::cout << make_response_error(seq, EXIT_RENDER_FAILURE, "bad render args") << std::endl;
            } else {
                try {
                    Heartbeat hb("render");
                    pool_render(ps, pe, out);
                    std::cout << make_response_render(seq, out) << std::endl;
                } catch (const WorkerError& e) {
                    std::cout << make_response_error(seq, e.exit_code(), e.what()) << std::endl;
                } catch (const std::bad_alloc&) {
                    std::cout << make_response_error(seq, EXIT_OOM, "render: bad_alloc") << std::endl;
                } catch (const std::exception& e) {
                    std::cout << make_response_error(seq, EXIT_RENDER_FAILURE,
                        std::string("render: ") + e.what()) << std::endl;
                } catch (...) {
                    std::cout << make_response_error(seq, EXIT_RENDER_FAILURE,
                        "render: non-standard exception") << std::endl;
                }
            }
            leader_busy = false;
            return true;
        }
        return false;
    };

    LineAccumulator stdin_acc;
    while (stdin_open || !pending.empty() || std::any_of(children.begin(), children.end(),
                                                          [](const ChildSlot& c) { return c.busy; })) {
        std::vector<pollfd> pfds;
        if (stdin_open) pfds.push_back({STDIN_FILENO, POLLIN, 0});
        for (auto& c : children) {
            if (!c.dead) pfds.push_back({c.sock_fd, POLLIN, 0});
        }

        int n = ::poll(pfds.data(), pfds.size(), -1);
        if (n < 0) {
            if (errno == EINTR) continue;
            break;
        }

        size_t idx = 0;
        if (stdin_open) {
            if (pfds[idx].revents & (POLLIN | POLLHUP)) {
                std::vector<std::string> lines;
                bool alive = stdin_acc.drain(STDIN_FILENO, lines);
                for (auto& cmd_line : lines) {
                    if (cmd_line.empty()) continue;
                    std::string cmd = json_string(cmd_line, "cmd");
                    if (cmd == "quit") {
                        quitting = true;
                        stdin_open = false;
                        break;
                    }
                    if (cmd == "render") {
                        if (!try_assign(cmd_line)) pending.push_back(std::move(cmd_line));
                    } else {
                        // Unknown command — emit an error response, keep going.
                        int seq = json_int(cmd_line, "seq");
                        std::cout << make_response_error(seq, EXIT_RENDER_FAILURE,
                            "unknown cmd: " + cmd) << std::endl;
                    }
                }
                if (!alive) stdin_open = false;
            }
            ++idx;
        }

        for (auto& c : children) {
            if (c.dead) continue;  // not in pfds (same skip as when building it)
            if (pfds[idx].revents & (POLLIN | POLLHUP | POLLERR)) {
                std::vector<std::string> lines;
                bool alive = c.acc.drain(c.sock_fd, lines);
                for (auto& resp_line : lines) {
                    if (resp_line.empty()) continue;
                    // Forward verbatim to orchestrator stdout
                    std::cout << resp_line << std::endl;
                    c.busy = false;
                    c.in_flight_seq = -1;
                    // Drain pending queue onto newly-free child
                    if (alive && !pending.empty() && dispatch_to_child(c, pending.front())) {
                        pending.pop_front();
                    }
                }
                if (!alive) retire_child(c);
            }
            ++idx;
        }

        // Children may have died with work queued; hand it to whatever slot is
        // free (another child, or the leader inline) instead of stalling.
        while (!pending.empty() && try_assign(pending.front())) {
            pending.pop_front();
        }

        // If quitting and all children idle, break out.
        if (quitting && !leader_busy &&
            std::all_of(children.begin(), children.end(),
                        [](const ChildSlot& c) { return !c.busy; })) {
            break;
        }
    }

    // Shutdown: signal each child to quit, close, reap.
    for (auto& c : children) {
        if (c.sock_fd >= 0) {
            fd_write_line(c.sock_fd, "{\"cmd\":\"quit\"}");
            ::shutdown(c.sock_fd, SHUT_WR);
        }
    }
    for (auto& c : children) {
        if (c.sock_fd >= 0) ::close(c.sock_fd);
        if (c.pid > 0) {
            int status = 0;
            ::waitpid(c.pid, &status, 0);
        }
    }
    return EXIT_OK;
}

int pool_loop(const std::string& format, const std::string& license_path) {
    // Apply license once at startup
    try {
        apply_license(license_path);
    } catch (const WorkerError& e) {
        respond_error(e.exit_code(), e.what());
        return e.exit_code();
    } catch (const std::exception& e) {
        respond_error(EXIT_LICENSE_INVALID, e.what());
        return EXIT_LICENSE_INVALID;
    }

    std::string line;
    bool document_loaded = false;

    while (std::getline(std::cin, line)) {
        if (line.empty()) continue;

        std::string cmd = json_string(line, "cmd");

        if (cmd == "quit") {
            return EXIT_OK;
        }

        if (cmd == "load") {
            std::string input = json_string(line, "input");
            if (input.empty()) {
                respond_error(EXIT_RENDER_FAILURE, "missing input path");
                continue;
            }
            try {
                Heartbeat hb("load");
                int page_count = pool_load(input);
                document_loaded = true;
                respond_ok(page_count);
            } catch (const WorkerError& e) {
                respond_error(e.exit_code(), e.what());
            } catch (const std::bad_alloc&) {
                respond_error(EXIT_OOM, "load: bad_alloc");
                return EXIT_OOM;  // OOM is fatal for the process
            } catch (const std::exception& e) {
                respond_error(EXIT_RENDER_FAILURE, std::string("load: ") + e.what());
            } catch (...) {
                // Aspose::Cells::CellsException does not derive from std::exception;
                // without this it unwinds to main and the worker exits mid-protocol.
                respond_error(EXIT_RENDER_FAILURE, "load: non-standard exception");
            }
            continue;
        }

        if (cmd == "render") {
            if (!document_loaded) {
                respond_error(EXIT_RENDER_FAILURE, "no document loaded");
                continue;
            }
            int page_start = json_int(line, "page_start");
            int page_end = json_int(line, "page_end");
            std::string output = json_string(line, "output");
            if (page_start < 1 || page_end < page_start || output.empty()) {
                respond_error(EXIT_RENDER_FAILURE, "bad render args");
                continue;
            }
            try {
                Heartbeat hb("render");
                pool_render(page_start, page_end, output);
                respond_ok_render(output);
            } catch (const WorkerError& e) {
                respond_error(e.exit_code(), e.what());
            } catch (const std::bad_alloc&) {
                respond_error(EXIT_OOM, "render: bad_alloc");
                return EXIT_OOM;  // OOM is fatal
            } catch (const std::exception& e) {
                respond_error(EXIT_RENDER_FAILURE, std::string("render: ") + e.what());
            } catch (...) {
                respond_error(EXIT_RENDER_FAILURE, "render: non-standard exception");
            }
            continue;
        }

        respond_error(EXIT_RENDER_FAILURE, "unknown cmd: " + cmd);
    }

    // stdin closed (orchestrator died or pipe broken)
    return EXIT_OK;
}

int current_pool_index() {
    return g_pool_index;
}

}  // namespace office_convert
