import type { NextRequest } from 'next/server';

// Single-origin streaming proxy to the Go API (BR-UI-1: the browser only ever
// talks to this UI's origin).
//
// WHY A ROUTE HANDLER, NOT next.config rewrites(): rewrites() buffered large
// multipart upload bodies and timed out at ~30s, returning HTTP 500 — a 47 MiB
// DOCX never reached the API intact (the API logged `missing_file`). A Node
// runtime route handler STREAMS both the request body (upload) and the response
// body (PDF download) straight through, so files of any size pass cleanly.
//
// runtime=nodejs: needed for streamed-body fetch (duplex) and to resolve the
// internal compose hostname (office-convert:8080). dynamic: read API_URL and
// proxy per-request (no build-time baking → API target is runtime-configurable).
export const runtime = 'nodejs';
export const dynamic = 'force-dynamic';

const API_URL = process.env.API_URL ?? 'http://office-convert:8080';

// Hop-by-hop / length headers that must not be forwarded verbatim when the body
// is re-streamed (chunked). Host is dropped so the upstream sees its own host.
// `expect` (100-continue, sent by curl/some clients for large uploads) is
// rejected by undici fetch with "expect header not supported" — must be dropped.
// Client-supplied forwarding headers are dropped too: the API rate-limits on
// X-Forwarded-For, so passing a browser's forged value through would let it
// pick its own bucket. Only the last hop's entry is re-forwarded (below).
const STRIP_REQUEST = new Set([
  'host',
  'connection',
  'content-length',
  'transfer-encoding',
  'expect',
  'x-forwarded-for',
  'x-real-ip',
  'forwarded',
]);
const STRIP_RESPONSE = new Set(['content-encoding', 'content-length', 'transfer-encoding', 'connection']);

async function proxy(request: NextRequest, path: string[]): Promise<Response> {
  const target = `${API_URL}/${path.join('/')}${request.nextUrl.search}`;

  const headers = new Headers();
  request.headers.forEach((value, key) => {
    if (!STRIP_REQUEST.has(key.toLowerCase())) headers.set(key, value);
  });
  // The rightmost X-Forwarded-For entry is the one appended by the proxy in
  // front of the UI (e.g. the ALB); everything left of it is client-controlled.
  const clientIp = (request.headers.get('x-forwarded-for') ?? '').split(',').pop()?.trim();
  if (clientIp) headers.set('x-forwarded-for', clientIp);

  const hasBody = request.method !== 'GET' && request.method !== 'HEAD';

  // `duplex: 'half'` is required by undici/Node when sending a streaming body.
  const init: RequestInit & { duplex?: 'half' } = {
    method: request.method,
    headers,
    redirect: 'manual',
  };
  if (hasBody) {
    init.body = request.body;
    init.duplex = 'half';
  }

  let upstream: Response;
  try {
    upstream = await fetch(target, init);
  } catch (err) {
    // Upstream unreachable / stream aborted — surface a clean 502 rather than a
    // generic 500 from the framework.
    // Log the detail server-side; return a generic message (no internal-error
    // disclosure to the client).
    const message = err instanceof Error ? `${err.name}: ${err.message}` : String(err);
    console.error('[api-proxy] fetch failed:', message, (err as { cause?: unknown })?.cause);
    return Response.json(
      { failure_class: 'engine_unavailable', detail: { message: 'API proxy could not reach the backend' } },
      { status: 502 },
    );
  }

  const responseHeaders = new Headers();
  upstream.headers.forEach((value, key) => {
    if (!STRIP_RESPONSE.has(key.toLowerCase())) responseHeaders.set(key, value);
  });
  // The API's HTML pages (e.g. /v1/dashboard) are meant to be loaded from the
  // API origin. Served through this proxy they would run on the UI's origin
  // without the middleware CSP (the matcher skips /api), so sandbox them into
  // an opaque origin.
  if ((upstream.headers.get('content-type') ?? '').includes('text/html')) {
    responseHeaders.set('content-security-policy', 'sandbox allow-scripts');
  }

  // Stream the upstream body back unbuffered (PDF downloads of any size).
  return new Response(upstream.body, {
    status: upstream.status,
    statusText: upstream.statusText,
    headers: responseHeaders,
  });
}

type RouteContext = { params: Promise<{ path: string[] }> };

export async function GET(request: NextRequest, ctx: RouteContext) {
  return proxy(request, (await ctx.params).path);
}

export async function POST(request: NextRequest, ctx: RouteContext) {
  return proxy(request, (await ctx.params).path);
}
