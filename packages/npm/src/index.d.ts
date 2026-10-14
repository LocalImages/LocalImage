import type { Browser } from 'playwright-core';
import type {
  BrowserHealth,
  BrowserName,
  CallOptions,
  CreateOptions,
  EnsureDaemonOptions,
  EnsureDaemonResult,
  ExecOptions,
  ExecResult,
  ExtensionInfo,
  FileInfo,
  Info,
  ListOptions,
  LocalImageOptions,
  LogSource,
  MouseButton,
  PrepareOptions,
  PrepareResult,
  Protocol,
  RecordingInfo,
  ScreenshotRegion,
  SessionInfo,
} from './types.js';

export * from './types.js';

/** Every code a LocalImageError can carry, for exhaustive mapping. */
export declare const ERROR_CODES: readonly [
  'session_gone',
  'limit_exceeded',
  'invalid_request',
  'conflict',
  'image_not_ready',
  'unauthorized',
  'forbidden',
  'docker_unavailable',
  'not_found',
  'internal',
  'daemon_unreachable',
  'daemon_start_failed',
  'binary_not_found',
  'request_timeout',
  'incompatible_daemon',
];

/** The daemon API revision this SDK speaks. */
export declare const SUPPORTED_API_VERSION: number;

/**
 * - `session_gone`: the session does not exist (deleted, expired, never created)
 * - `limit_exceeded`: too many sessions, or a ttl over the limit (`details`: `max`, `running`)
 * - `invalid_request`: malformed, or it names something that does not exist
 * - `conflict`: clashes with current state, for example a profile already in use
 * - `image_not_ready`: `noBuild` was set and the browser image is not built
 * - `unauthorized`: missing or wrong API key
 * - `forbidden`: the request's Host or Origin is not allowed (see LOCALIMAGE_ALLOWED_HOSTS)
 * - `docker_unavailable`: the daemon cannot reach Docker
 * - `not_found`, `internal`: anything else from the daemon
 * - `daemon_unreachable`: nothing is listening at the daemon address, or the connection was lost
 * - `daemon_start_failed`: ensureDaemon could not start the daemon
 * - `binary_not_found`: there is no localimage binary for this platform
 * - `request_timeout`: the call exceeded its `timeoutMs`
 * - `incompatible_daemon`: assertCompatible() found a daemon this SDK cannot talk to
 * - `http_<status>`: a response that was not the daemon's own JSON error
 */
export type LocalImageErrorCode = (typeof ERROR_CODES)[number] | `http_${number}`;

/**
 * The only error this SDK throws for a failed call. An aborted call is different: it rejects
 * with the AbortSignal's reason (normally a DOMException named `AbortError`).
 */
export declare class LocalImageError extends Error {
  constructor(
    message: string,
    init?: { code?: LocalImageErrorCode; status?: number; details?: Record<string, number> },
  );
  readonly name: 'LocalImageError';
  code: LocalImageErrorCode;
  /** HTTP status, for errors that came from the daemon. */
  status?: number | undefined;
  /** For `limit_exceeded`: `{ max, running }`, or `{ max, requested }` from assertCapacity(). */
  details?: Record<string, number> | undefined;
}

/** The module passed to Session.connect(): `await import('playwright-core')`. */
export type PlaywrightModule = typeof import('playwright-core');

/** A running browser session, as returned by LocalImage.create(). A plain object: it is safe to
 * rebuild one from a stored lease by calling list() and using the ids and URLs it reports. */
export interface Session {
  id: string;
  browser: BrowserName;
  protocol: Protocol;
  /** Attach with chromium.connectOverCDP(connectUrl) or firefox.connect(connectUrl), or use connect(). */
  connectUrl: string;
  /** Interactive live view: whoever opens it can control the browser. */
  liveUrl: string;
  /** Read-only live view. */
  viewUrl: string;
  profile: string | undefined;
  extensions: string[];
  tags: Record<string, string>;
  /** Where the browser saves downloads. */
  downloadsDir: string;
  expiresAt: Date;

  /** Attach with playwright-core (a peer dependency), or the module you pass in. */
  connect(pw?: PlaywrightModule): Promise<Browser>;
  /** PNG of the display, or of `region`. */
  screenshot(region?: ScreenshotRegion, opts?: CallOptions): Promise<Buffer>;

  // Computer controls act on the whole display, in screen coordinates (not page coordinates).
  click(x: number, y: number, opts?: { button?: MouseButton; numClicks?: number }): Promise<null>;
  moveMouse(x: number, y: number): Promise<null>;
  type(text: string, opts?: { delayMs?: number }): Promise<null>;
  pressKey(...keys: string[]): Promise<null>;
  scroll(x: number, y: number, opts?: { deltaX?: number; deltaY?: number }): Promise<null>;
  drag(path: Array<[number, number]>, opts?: { button?: MouseButton }): Promise<null>;

  /** Begin a recording. Resolves once frames are being written. One can run at a time. */
  startRecording(opts?: CallOptions & { maxSeconds?: number | undefined }): Promise<RecordingInfo>;
  /** Stop the running recording (or `replayId`). The file is complete when this resolves. */
  stopRecording(opts?: CallOptions & { replayId?: string }): Promise<RecordingInfo>;
  recordings(opts?: CallOptions): Promise<RecordingInfo[]>;
  /** MP4 bytes of a recording; the latest if `replayId` is omitted. */
  downloadRecording(replayId?: string, opts?: CallOptions): Promise<Buffer>;

  /** Run a command in the session container as the browser user (no shell). */
  exec(command: string[], opts?: ExecOptions): Promise<ExecResult>;
  uploadFile(path: string, data: string | Uint8Array, opts?: CallOptions): Promise<{ size: number }>;
  /**
   * The end of one of the session's logs, as text (default source `container`, 200 lines). Works for
   * a stopped session too, to see why it died.
   */
  logs(opts?: CallOptions & { source?: LogSource | undefined; tail?: number | undefined }): Promise<string>;
  /** Read a file from the session, for example a download (see downloadsDir). */
  downloadFile(path: string, opts?: CallOptions): Promise<Buffer>;
  listFiles(path: string, opts?: CallOptions): Promise<FileInfo[]>;

  /**
   * Whether the browser is up and whether it has restarted. After a crash it comes back by itself on
   * the same URL, but as a new, empty browser: `restarts` going up is how a client finds out.
   */
  health(opts?: CallOptions): Promise<BrowserHealth>;
  /** Change tags after creation. Resolves to the resulting tags and updates `tags` on this object. */
  updateTags(opts?: CallOptions & { set?: Record<string, string> | undefined; remove?: string[] | undefined }): Promise<Record<string, string>>;
  /** Write every log source to `dir/<id>/<source>.log`; resolves to the file paths. Call before close(). */
  saveLogs(dir: string, opts?: CallOptions): Promise<string[]>;
  /** Delete the session. Safe to call more than once; resolves true if this call removed it. */
  close(opts?: CallOptions): Promise<boolean>;
}

/**
 * Make sure a daemon is running at `addr`, starting one if needed. A running daemon is reused.
 * Rejects with `daemon_start_failed`, `docker_unavailable` or `binary_not_found`.
 */
export declare function ensureDaemon(opts?: EnsureDaemonOptions): Promise<EnsureDaemonResult>;

export declare class LocalImage {
  constructor(opts?: LocalImageOptions);
  readonly addr: string;
  readonly apiKey: string | undefined;
  autoStart: boolean;
  daemonEnv: Record<string, string>;

  /** Start the daemon if it is not running (see the exported ensureDaemon). */
  ensureDaemon(opts?: EnsureDaemonOptions): Promise<EnsureDaemonResult>;
  /** Stop the daemon. Resolves true if one was stopped, false if none was running. */
  shutdown(opts?: CallOptions): Promise<boolean>;
  /** Generate and activate a new daemon API key; the previous key stops working immediately. */
  rotateKey(opts?: CallOptions): Promise<string>;
  /** The daemon's version, limits (`maxSessions`) and load. */
  info(opts?: CallOptions): Promise<Info>;
  /**
   * Reject with `limit_exceeded` (details `{ max, requested }`) when `n` sessions could never run
   * at once, so a worker pool can fail early. Resolves to info().
   */
  assertCapacity(n: number, opts?: CallOptions): Promise<Info>;
  /**
   * Reject with `incompatible_daemon` unless the daemon speaks this SDK's API, and, if given, its
   * browsers match (a string for an exact version, or a RegExp). Resolves to info().
   */
  assertCompatible(
    opts?: CallOptions & { chromium?: string | RegExp | undefined; firefox?: string | RegExp | undefined },
  ): Promise<Info>;
  /** Build missing browser images ahead of time, so create() does not have to. */
  prepare(opts?: PrepareOptions): Promise<PrepareResult>;

  /** Remove browser images left behind by older versions. Resolves to the tags removed. */
  prune(opts?: CallOptions): Promise<string[]>;
  create(opts?: CreateOptions): Promise<Session>;
  /**
   * A Session for one that already exists, by id: from another process, after a restart, or from a
   * stored lease. Rejects with `session_gone`. A stopped session can be attached to read its logs.
   */
  attach(id: string, opts?: CallOptions): Promise<Session>;
  /** Sessions in every state, including stopped ones a crashed worker left behind. */
  list(opts?: ListOptions): Promise<SessionInfo[]>;
  /** Resolves true if this call removed the session, false if it was already gone. Never throws for that. */
  delete(id: string, opts?: CallOptions): Promise<boolean>;

  uploadExtension(name: string, zip: Uint8Array): Promise<ExtensionInfo>;
  extensions(): Promise<ExtensionInfo[]>;
  deleteExtension(name: string): Promise<null>;
  profiles(): Promise<string[]>;
  deleteProfile(name: string): Promise<null>;
}
