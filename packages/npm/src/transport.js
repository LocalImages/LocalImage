import http from 'node:http';
import https from 'node:https';
import { LocalImageError } from './errors.js';

// Node's built-in fetch gives up on a response after 5 minutes of silence, and reports it as a
// generic network failure. A first-run image build is one long request, so the SDK talks HTTP
// directly: no hidden ceiling, an AbortSignal, an optional per-call timeout, and streaming.

const UNREACHABLE = new Set(['ECONNREFUSED', 'ENOTFOUND', 'EHOSTUNREACH', 'ENETUNREACH', 'ECONNRESET', 'EPIPE']);

function splitAddr(addr) {
  const url = new URL(addr.includes('://') ? addr : `http://${addr}`);
  return { hostname: url.hostname, port: Number(url.port) || (url.protocol === 'https:' ? 443 : 80), protocol: url.protocol };
}

const abortReason = (signal) => signal.reason ?? new DOMException('This operation was aborted', 'AbortError');

/**
 * One request to the daemon.
 *
 * @param {object} [opts]
 * @param {boolean} [opts.raw] resolve to a Buffer instead of parsed JSON
 * @param {AbortSignal} [opts.signal] cancel the call; the daemon cleans up what it had started
 * @param {number} [opts.timeoutMs] give up after this long in total (code `request_timeout`)
 * @param {(event: object) => void} [opts.onLine] the response is newline-delimited JSON; call this
 *   for each object as it arrives. Resolves to undefined.
 */
export function request(addr, apiKey, method, path, body, { raw = false, signal, timeoutMs, onLine } = {}) {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(abortReason(signal));

    const isBinary = body instanceof Uint8Array;
    const payload = body === undefined ? null : isBinary ? Buffer.from(body) : Buffer.from(JSON.stringify(body));
    const { hostname, port, protocol } = splitAddr(addr);

    let settled = false;
    let timer;
    const finish = (fn, value) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      signal?.removeEventListener('abort', onAbort);
      fn(value);
    };

    const request = protocol === 'https:' ? https.request : http.request;
    const req = request(
      {
        hostname,
        port,
        method,
        path,
        headers: {
          'Content-Type': isBinary ? 'application/octet-stream' : 'application/json',
          ...(payload ? { 'Content-Length': payload.length } : {}),
          ...(apiKey ? { Authorization: `Bearer ${apiKey}` } : {}),
        },
      },
      (res) => {
        const chunks = [];
        let pending = '';
        res.on('data', (chunk) => {
          if (onLine && res.statusCode < 400) {
            pending += chunk.toString('utf8');
            let nl;
            while ((nl = pending.indexOf('\n')) >= 0) {
              const line = pending.slice(0, nl).trim();
              pending = pending.slice(nl + 1);
              if (line) {
                try {
                  onLine(JSON.parse(line));
                } catch (e) {
                  if (e instanceof SyntaxError) continue;
                  return req.destroy(e); // an error thrown by the caller's callback
                }
              }
            }
          } else {
            chunks.push(chunk);
          }
        });
        res.on('error', (e) => finish(reject, connectionLost(addr, e)));
        res.on('aborted', () => finish(reject, connectionLost(addr, { code: 'ECONNRESET' })));
        res.on('end', () => {
          const buf = Buffer.concat(chunks);
          if (res.statusCode >= 400) {
            let b = {};
            try {
              b = JSON.parse(buf.toString('utf8'));
            } catch {
              /* not JSON */
            }
            return finish(
              reject,
              new LocalImageError(b.error ?? `HTTP ${res.statusCode}`, {
                code: b.code ?? `http_${res.statusCode}`,
                status: res.statusCode,
                details: b.details,
              }),
            );
          }
          if (onLine) return finish(resolve, undefined);
          if (raw) return finish(resolve, buf);
          if (res.statusCode === 204 || buf.length === 0) return finish(resolve, null);
          try {
            finish(resolve, JSON.parse(buf.toString('utf8')));
          } catch (e) {
            finish(reject, new LocalImageError(`the daemon sent a response that is not JSON: ${e.message}`));
          }
        });
      },
    );

    req.on('error', (e) => {
      if (e instanceof LocalImageError || e?.name === 'AbortError' || e === signal?.reason) return finish(reject, e);
      finish(reject, connectionLost(addr, e));
    });

    function onAbort() {
      req.destroy(abortReason(signal));
    }
    signal?.addEventListener('abort', onAbort, { once: true });

    if (timeoutMs) {
      timer = setTimeout(
        () => req.destroy(new LocalImageError(`${method} ${path} did not finish within ${timeoutMs} ms`, { code: 'request_timeout' })),
        timeoutMs,
      );
    }

    req.end(payload ?? undefined);
  });
}

function connectionLost(addr, e) {
  const code = e?.code ?? e?.cause?.code ?? e?.message;
  const why = UNREACHABLE.has(code) ? '' : ` (${code})`;
  return new LocalImageError(
    `localimage daemon not reachable at ${addr}; start it with \`localimage start\`, or use ensureDaemon() / autoStart${why}`,
    { code: 'daemon_unreachable' },
  );
}
