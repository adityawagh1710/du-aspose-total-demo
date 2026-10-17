# Document Convert

Converts Office documents, images, email (EML) and HTML to PDF over an HTTP API.

- **Backend:** Go orchestrator (`cmd/`, `internal/`) driving Aspose.Total C++ workers (`worker_cpp/`). Documents render in RAM-capped chunks, and qpdf streams the merged PDF.
- **HTML:** two engines, Gotenberg (Chromium, runs JavaScript) and Aspose.Words (static markup).
- **UI:** Next.js operator dashboard (`ui/`).

**Status:** v1 local proof of concept.

## Prerequisites

- Docker with Compose v2, on an x86_64 host.
- Aspose.Total for C++ libraries in `vendor/aspose/` (check with `make verify-vendor`).
- An Aspose.Total **for C++** license at `./Aspose.TotalforC++.lic` ([temporary license](https://purchase.aspose.com/temporary-license)).

## Quickstart

```bash
make up        # build and start the API, UI, Gotenberg and LocalStack
curl -X POST http://localhost:8080/v1/convert -F "file=@testdata/corpus/simple.pdf" -o out.pdf
make down
```

- API: http://localhost:8080 (`GET /health`)
- Dashboard: http://localhost:8501
- All `make` targets: `make help`

## API

| Endpoint | Purpose |
| --- | --- |
| `POST /v1/convert` | Office files, images or EML to PDF |
| `POST /v1/convert/html/gotenberg` | HTML to PDF with Chromium |
| `POST /v1/convert/html/aspose` | HTML to PDF with Aspose.Words |
| `GET /health` | Readiness and license status |

Errors come back as JSON: `{request_id, failure_class, detail}`.

## Development

```bash
make test-go           # Go tests, including the golden parity gate
npm --prefix ui test   # UI tests
```

Configuration uses `OFFICE_CONVERT_*` environment variables (see `compose.yaml`). Design docs are in `aidlc-docs/`, and EKS deployment is in `deploy/README.md`.

Aspose.Total requires a separate commercial license from Aspose.
