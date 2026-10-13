/**
 * Every failure is a LocalImageError with a stable `code`, so callers can branch without parsing
 * messages. Codes from the daemon:
 *
 *   session_gone        the session does not exist (already deleted, expired, never created)
 *   limit_exceeded      too many sessions, or a ttl over the limit; `details` has { max, running }
 *   invalid_request     malformed request, or it names something that does not exist
 *   conflict            clashes with current state (for example a profile already in use)
 *   image_not_ready     noBuild was set and the browser image is not built yet (see prepare())
 *   unauthorized        missing or wrong API key
 *   docker_unavailable  the daemon cannot reach Docker
 *   not_found, internal
 *
 * Codes raised by this SDK:
 *
 *   daemon_unreachable  nothing is listening at the daemon address, or the connection was lost
 *   daemon_start_failed ensureDaemon could not start the daemon (the message has its log)
 *   binary_not_found    there is no localimage binary for this platform
 *   request_timeout     the call exceeded its `timeoutMs`
 *
 * An aborted call (an AbortSignal fired) rejects with the signal's reason, normally an
 * AbortError, not a LocalImageError, so callers can tell "I cancelled" from "it failed".
 */
export class LocalImageError extends Error {
  constructor(message, { code = 'internal', status, details } = {}) {
    super(message);
    this.name = 'LocalImageError';
    this.code = code;
    this.status = status;
    this.details = details;
  }
}

/**
 * Every error code the SDK can produce, so a provider can map them exhaustively. An error that
 * did not come from the daemon's JSON (a proxy or an unexpected body) has code `http_<status>`.
 */
export const ERROR_CODES = Object.freeze([
  // from the daemon
  'session_gone',
  'limit_exceeded',
  'invalid_request',
  'conflict',
  'image_not_ready',
  'unauthorized',
  'docker_unavailable',
  'not_found',
  'internal',
  // raised by the SDK
  'daemon_unreachable',
  'daemon_start_failed',
  'binary_not_found',
  'request_timeout',
]);
