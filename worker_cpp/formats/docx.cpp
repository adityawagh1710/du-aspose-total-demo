// docx.cpp — Aspose.Words-only TU. Owns apply_license, dispatch_render,
// dispatch_probe, pool_load, pool_render for the office-convert-worker-docx binary.
//
// Page-range subsetting via PdfSaveOptions::set_PageSet with explicit
// zero-based page indices.
//
// This binary also renders format=html (single-shot, full document): Words
// loads HTML natively, so HTML→PDF reuses the docx worker instead of shipping
// a sixth Aspose product. The html path ignores --page-range and enforces the
// BR-4 external-resource deny policy via IResourceLoadingCallback.

#include <Aspose.Words.Cpp/Document.h>
#include <Aspose.Words.Cpp/Licensing/License.h>
#include <Aspose.Words.Cpp/LoadFormat.h>
#include <Aspose.Words.Cpp/Loading/DocumentLoadingArgs.h>
#include <Aspose.Words.Cpp/Loading/IDocumentLoadingCallback.h>
#include <Aspose.Words.Cpp/Loading/IResourceLoadingCallback.h>
#include <Aspose.Words.Cpp/Loading/LoadOptions.h>
#include <Aspose.Words.Cpp/Loading/ResourceLoadingAction.h>
#include <Aspose.Words.Cpp/Loading/ResourceLoadingArgs.h>
#include <Aspose.Words.Cpp/PageSetup.h>
#include <Aspose.Words.Cpp/Saving/PageSet.h>
#include <Aspose.Words.Cpp/Saving/PdfSaveOptions.h>
#include <Aspose.Words.Cpp/Section.h>
#include <Aspose.Words.Cpp/SectionCollection.h>
#include <system/array.h>
#include <system/object.h>
#include <system/string.h>

#include <arpa/inet.h>
#include <netinet/in.h>

#include <cctype>
#include <chrono>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <iostream>
#include <sstream>
#include <string>

#include "../error.h"
#include "../license.h"
#include "../pool.h"
#include "../probe.h"
#include "../probe_util.h"
#include "../render.h"
#include "../timing_util.h"

namespace office_convert {

namespace {

constexpr const char* kFormat = "docx";

// Module-level document handle for pool mode.
System::SharedPtr<Aspose::Words::Document> g_document;

// Aspose.Words' LoadFormat::Auto fails to detect ODT in this C++ build —
// the loader expects OOXML structure when handed a zip-magic file and raises
// FileCorruptedException on the ODF layout. Force the LoadFormat::Odt hint
// when the input filename advertises ODF Text (.odt) or its template (.ott).
bool input_ends_with_odt(const std::string& path) {
    auto ends = [&](const char* suffix) {
        size_t n = std::strlen(suffix);
        return path.size() >= n
            && path.compare(path.size() - n, n, suffix) == 0;
    };
    return ends(".odt") || ends(".ott");
}

void verify_license_file(const std::string& path) {
    FILE* f = std::fopen(path.c_str(), "rb");
    if (f == nullptr) {
        throw LicenseException("license file not readable: " + path);
    }
    std::fclose(f);
}

void render_docx_from(System::SharedPtr<Aspose::Words::Document> doc,
                      int page_start, int page_end,
                      const std::string& output) {
    const int count = page_end - page_start + 1;
    auto pages = System::MakeArray<int32_t>(count);
    for (int i = 0; i < count; ++i) {
        pages[i] = page_start - 1 + i;  // 0-based indices
    }
    auto page_set = System::MakeObject<Aspose::Words::Saving::PageSet>(pages);

    auto opts = System::MakeObject<Aspose::Words::Saving::PdfSaveOptions>();
    opts->set_PageSet(page_set);
    opts->set_TempFolder(System::String("/tmp"));
    opts->set_MemoryOptimization(true);

    doc->Save(System::String(output.c_str()), opts);
}

System::SharedPtr<Aspose::Words::Loading::LoadOptions> new_load_options(const std::string& input);

void render_docx(const RenderArgs& args) {
    auto load_opts = new_load_options(args.input);
    auto doc = System::MakeObject<Aspose::Words::Document>(
        System::String(args.input.c_str()), load_opts);
    render_docx_from(doc, args.page_start, args.page_end, args.output);
}

// --- HTML render path (format=html) ---

// BR-4 deny policy (functional-design business-rules.md): external resources
// referenced by uploaded HTML are skipped unless they are http(s) URLs to
// public hosts. Loopback, RFC1918, link-local/metadata, IPv6-private, and
// single-label (in-cluster) hostnames are denied. Mirrors the orchestrator's
// internal/netpolicy and Gotenberg's --chromium-deny-list — keep in sync.
// 0/8, 10/8, 127/8, 169.254/16, 172.16/12, 192.168/16 (host byte order).
bool private_ipv4(uint32_t addr) {
    const uint32_t a = addr >> 24, b = (addr >> 16) & 0xff;
    return a == 0 || a == 10 || a == 127 || (a == 169 && b == 254) ||
           (a == 172 && b >= 16 && b <= 31) || (a == 192 && b == 168);
}

bool numeric_label(const std::string& label) {
    if (label.rfind("0x", 0) == 0) return true;
    if (label.empty()) return false;
    for (char c : label) {
        if (!std::isdigit(static_cast<unsigned char>(c))) return false;
    }
    return true;
}

bool deny_resource_url(const std::string& raw, std::string& reason) {
    std::string lower;
    lower.reserve(raw.size());
    for (char c : raw) lower.push_back(static_cast<char>(std::tolower(static_cast<unsigned char>(c))));

    // data: URIs embed their payload — no network fetch, allow.
    if (lower.rfind("data:", 0) == 0) return false;

    const auto scheme_end = lower.find("://");
    if (scheme_end == std::string::npos) {
        reason = "non-absolute or non-http(s) resource";
        return true;
    }
    const std::string scheme = lower.substr(0, scheme_end);
    if (scheme != "http" && scheme != "https") {
        reason = "scheme " + scheme;
        return true;
    }

    // Authority ends at the first '/', '?', '#' (or '\', which browsers treat
    // as '/'); only then strip userinfo, up to the LAST '@'. Searching for '@'
    // first let "http://169.254.169.254/x/@a.com" pass as host "a.com".
    std::string host = lower.substr(scheme_end + 3);
    if (auto end = host.find_first_of("/?#\\"); end != std::string::npos) host = host.substr(0, end);
    if (auto at = host.rfind('@'); at != std::string::npos) host = host.substr(at + 1);
    if (!host.empty() && host.front() == '[') {  // IPv6 literal
        const auto close = host.find(']');
        std::string v6 = close == std::string::npos ? host.substr(1) : host.substr(1, close - 1);
        if (auto zone = v6.find('%'); zone != std::string::npos) v6 = v6.substr(0, zone);
        // inet_pton accepts every textual form (::1, 0:0:0:0:0:0:0:1, ::ffff:7f00:1).
        in6_addr a6{};
        if (inet_pton(AF_INET6, v6.c_str(), &a6) != 1) {
            reason = "malformed IPv6 literal";
            return true;
        }
        const uint8_t* b = a6.s6_addr;
        bool zero10 = true;
        for (int i = 0; i < 10; ++i) zero10 = zero10 && b[i] == 0;
        // ::/96 (::, ::1, IPv4-compatible) and ::ffff:0:0/96 (IPv4-mapped).
        const bool reserved = zero10 && ((b[10] == 0 && b[11] == 0) || (b[10] == 0xff && b[11] == 0xff));
        if (reserved || (b[0] & 0xfe) == 0xfc || (b[0] == 0xfe && (b[1] & 0xc0) == 0x80)) {
            reason = "private IPv6 " + v6;
            return true;
        }
        return false;
    }
    if (auto colon = host.find(':'); colon != std::string::npos) host = host.substr(0, colon);
    // "localhost." resolves exactly like "localhost".
    while (!host.empty() && host.back() == '.') host.pop_back();

    if (host.empty()) {
        reason = "empty host";
        return true;
    }
    if (host == "localhost" || (host.size() > 10 && host.compare(host.size() - 10, 10, ".localhost") == 0)) {
        reason = "loopback host";
        return true;
    }
    // inet_aton accepts every legacy IPv4 spelling a resolver would honour
    // (127.1, 0x7f.0.0.1, 0177.0.0.1, 2130706433), not only dotted-quad.
    in_addr a4{};
    if (inet_aton(host.c_str(), &a4) != 0) {
        if (private_ipv4(ntohl(a4.s_addr))) {
            reason = "private IPv4 " + host;
            return true;
        }
        return false;
    }
    if (host.find('.') == std::string::npos) {
        reason = "single-label host " + host;
        return true;
    }
    if (numeric_label(host.substr(host.rfind('.') + 1))) {
        reason = "non-canonical IP literal " + host;
        return true;
    }
    return false;
}

class DenyPolicyResourceCallback
    : public Aspose::Words::Loading::IResourceLoadingCallback {
public:
    Aspose::Words::Loading::ResourceLoadingAction ResourceLoading(
        System::SharedPtr<Aspose::Words::Loading::ResourceLoadingArgs> args) override {
        const std::string uri = args->get_OriginalUri().ToUtf8String();
        std::string reason;
        if (deny_resource_url(uri, reason)) {
            // Hostname-level audit line (NFR-4); never the full URI or content.
            std::cerr << "{\"type\":\"resource_denied\",\"reason\":\"" << reason << "\"}\n"
                      << std::flush;
            return Aspose::Words::Loading::ResourceLoadingAction::Skip;
        }
        return Aspose::Words::Loading::ResourceLoadingAction::Default;
    }
};

// Every Words load gets the BR-4 deny callback, not only format=html: DOCX
// external linked images and the email pipeline's attacker-controlled MHTML
// (rendered as docx) fetch remote resources too.
System::SharedPtr<Aspose::Words::Loading::LoadOptions> new_load_options(const std::string& input) {
    auto opts = System::MakeObject<Aspose::Words::Loading::LoadOptions>();
    opts->set_ResourceLoadingCallback(System::MakeObject<DenyPolicyResourceCallback>());
    if (input_ends_with_odt(input)) {
        opts->set_LoadFormat(Aspose::Words::LoadFormat::Odt);
    }
    return opts;
}

// BR-7 fair-comparison geometry: US Letter, 0.5in margins, on every section
// (both engines render to identical page geometry). 1in == 72pt.
void force_letter_geometry(System::SharedPtr<Aspose::Words::Document> doc) {
    auto sections = doc->get_Sections();
    for (int i = 0; i < sections->get_Count(); ++i) {
        auto ps = sections->idx_get(i)->get_PageSetup();
        ps->set_PageWidth(612.0);   // 8.5in
        ps->set_PageHeight(792.0);  // 11in
        ps->set_TopMargin(36.0);    // 0.5in
        ps->set_BottomMargin(36.0);
        ps->set_LeftMargin(36.0);
        ps->set_RightMargin(36.0);
    }
}

// Full-document HTML render. --page-range is deliberately ignored: HTML has no
// pre-known page count and this path is single-shot (no chunk planner).
void render_html(const RenderArgs& args) {
    auto load_opts = new_load_options(args.input);
    load_opts->set_LoadFormat(Aspose::Words::LoadFormat::Html);
    auto doc = System::MakeObject<Aspose::Words::Document>(
        System::String(args.input.c_str()), load_opts);
    force_letter_geometry(doc);

    auto opts = System::MakeObject<Aspose::Words::Saving::PdfSaveOptions>();
    opts->set_TempFolder(System::String("/tmp"));
    opts->set_MemoryOptimization(true);
    doc->Save(System::String(args.output.c_str()), opts);
}

int probe_docx_page_count(const std::string& input) {
    auto load_opts = new_load_options(input);
    auto doc = System::MakeObject<Aspose::Words::Document>(
        System::String(input.c_str()), load_opts);
    return doc->get_PageCount();
}

}  // namespace

void apply_license(const std::string& license_path) {
    verify_license_file(license_path);
    try {
        auto license = System::MakeObject<Aspose::Words::License>();
        license->SetLicense(System::String(license_path.c_str()));
    } catch (const std::exception& e) {
        throw LicenseException(std::string("Aspose::Words SetLicense: ") + e.what());
    }
}

void dispatch_render(const RenderArgs& args) {
    if (args.format != kFormat && args.format != "html") {
        throw InputUnprocessableException("worker-docx invoked with format=" + args.format);
    }
    try {
        if (args.format == "html") {
            render_html(args);
        } else {
            render_docx(args);
        }
    } catch (const WorkerError&) {
        throw;
    } catch (const std::bad_alloc&) {
        throw OOMException("DOCX render bad_alloc");
    } catch (const std::exception& e) {
        throw RenderException(std::string("DOCX: ") + e.what());
    }
}

void dispatch_probe(const ProbeArgs& args) {
    if (args.format != kFormat) {
        throw InputUnprocessableException("worker-docx invoked with format=" + args.format);
    }
    int page_count = 0;
    try {
        page_count = probe_docx_page_count(args.input);
    } catch (const WorkerError&) {
        throw;
    } catch (const std::bad_alloc&) {
        throw OOMException("probe bad_alloc");
    } catch (const std::exception& e) {
        throw InputUnprocessableException(std::string("probe: ") + e.what());
    }
    emit_probe_json(page_count, kFormat, file_size_bytes(args.input));
}

// --- Pool mode ---

namespace {

// Aspose.Words load-progress callback. Fires throughout the Document
// constructor as the SDK parses the OOXML zip + WordML tree. EstimatedProgress
// returns 0.0..1.0. We throttle to integer-percent boundaries so a 100 MB
// load doesn't emit thousands of near-identical lines.
class LoadProgressCallback : public Aspose::Words::Loading::IDocumentLoadingCallback {
public:
    void Notify(System::SharedPtr<Aspose::Words::Loading::DocumentLoadingArgs> args) override {
        const double p = args->get_EstimatedProgress();
        const int pct = static_cast<int>(p * 100.0);
        if (pct == last_emitted_pct_) return;
        last_emitted_pct_ = pct;
        std::ostringstream oss;
        oss << "{\"type\":\"load_progress\",\"pool_index\":" << current_pool_index()
            << ",\"value\":" << (pct / 100.0) << "}\n";
        std::cerr << oss.str() << std::flush;
    }
private:
    int last_emitted_pct_ = -1;
};

}  // namespace

int pool_load(const std::string& input_path) {
    auto t0 = std::chrono::steady_clock::now();
    auto load_opts = new_load_options(input_path);
    auto cb = System::MakeObject<LoadProgressCallback>();
    load_opts->set_ProgressCallback(cb);
    g_document = System::MakeObject<Aspose::Words::Document>(
        System::String(input_path.c_str()), load_opts);
    auto t1 = std::chrono::steady_clock::now();
    emit_timing_ms("pool_load.document_load", t1 - t0);

    auto t2 = std::chrono::steady_clock::now();
    int page_count = g_document->get_PageCount();
    auto t3 = std::chrono::steady_clock::now();
    emit_timing_ms("pool_load.pagination", t3 - t2);

    return page_count;
}

void pool_render(int page_start, int page_end, const std::string& output_path) {
    if (!g_document) {
        throw RenderException("pool_render: no document loaded");
    }

    auto t0 = std::chrono::steady_clock::now();
    render_docx_from(g_document, page_start, page_end, output_path);
    auto t1 = std::chrono::steady_clock::now();
    emit_timing_ms("pool_render.save", t1 - t0);

    const int pages_in_chunk = page_end - page_start + 1;
    const long save_ms =
        std::chrono::duration_cast<std::chrono::milliseconds>(t1 - t0).count();
    emit_render_summary(pages_in_chunk, page_start, page_end, save_ms);
}

}  // namespace office_convert
