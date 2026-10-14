// Data shapes returned by, and passed to, the localimage SDK.

export type BrowserName = 'chromium' | 'firefox';

/** `cdp`: attach with chromium.connectOverCDP. `playwright`: attach with firefox.connect. */
export type Protocol = 'cdp' | 'playwright';

/** Options every network call accepts. */
export interface CallOptions {
  /** Cancel the call. It rejects with the signal's reason (normally an AbortError). */
  signal?: AbortSignal | undefined;
  /** Give up after this many milliseconds, rejecting with code `request_timeout`. */
  timeoutMs?: number | undefined;
}

/** A session as the daemon reports it, from list(). Field names are the daemon's (snake_case). */
export interface SessionInfo {
  id: string;
  browser: BrowserName;
  protocol: Protocol;
  connect_url: string;
  /** Interactive live view: whoever opens it can control the browser. */
  live_url: string;
  /** Read-only live view. */
  view_url: string;
  profile?: string | undefined;
  extensions?: string[] | undefined;
  tags?: Record<string, string> | undefined;
  /** Where the browser saves downloads; read files with downloadFile(). */
  downloads_dir: string;
  /** Docker state: `running`, `exited`, `created`, ... A crashed worker can leave `exited` ones. */
  state: string;
  /** Docker's verdict from a check covering the live view and the browser. */
  health?: 'starting' | 'healthy' | 'unhealthy' | undefined;
  /** RFC 3339 timestamps. */
  created_at: string;
  expires_at: string;
}

export interface Info {
  version: string;
  /** LOCALIMAGE_MAX_SESSIONS: how many sessions can run at once. */
  maxSessions: number;
  maxTtlSeconds: number;
  /** Sessions counted against the limit right now. */
  running: number;
  available: number;
  /** Which browser images are built; a missing one is built on first use (see prepare()). */
  images: Partial<Record<BrowserName, boolean>>;
  /** Old browser images on disk; prune() removes them. */
  staleImages: number;
  /** The daemon's API revision. assertCompatible() checks it against SUPPORTED_API_VERSION. */
  apiVersion: number;
  /** The browser versions inside the images, for example `{ chromium: { version: '154.0.8037.57' } }`. */
  components: Partial<Record<BrowserName, { version: string; playwright?: string | undefined }>>;
}

export interface CreateOptions extends CallOptions {
  /** Default `chromium`. */
  browser?: BrowserName | undefined;
  /** Display size, for example `1440x900@30` (the default). */
  screen?: string | undefined;
  /** Lifetime in seconds; the session is removed after it. Default 1800. */
  ttlSeconds?: number | undefined;
  /** Named persistent browser state. Chromium only; one live session per profile. */
  profile?: string | undefined;
  memoryMb?: number | undefined;
  cpus?: number | undefined;
  /** Chromium only: no tabs or address bar. */
  kiosk?: boolean | undefined;
  /** Names of uploaded extensions to load. Chromium only. */
  extensions?: string[] | undefined;
  /** Up to 20 key/value pairs stored on the session; filter list() by them. */
  tags?: Record<string, string> | undefined;
  /** Enable WebGL through software rendering. Chromium only. */
  webgl?: boolean | undefined;
  /** Browser timezone, an IANA name such as `Europe/Berlin`. Default UTC. */
  timezone?: string | undefined;
  /** Browser language, such as `de-DE`. Chromium only (set it on the Playwright context for Firefox). */
  locale?: string | undefined;
  /** Queue for a free session slot for this many seconds instead of failing at once. */
  waitSeconds?: number | undefined;
  /** Fail with `image_not_ready` instead of building a missing browser image. */
  noBuild?: boolean | undefined;
}

export interface ListOptions extends CallOptions {
  /** Keep only sessions that carry all of these tags. */
  tags?: Record<string, string> | undefined;
}

export interface PrepareOptions extends CallOptions {
  /** Default `['chromium']`. */
  browsers?: BrowserName[] | undefined;
  onProgress?: ((event: { browser: BrowserName; message: string }) => void) | undefined;
}

/** `ready`: the image already existed. `built`: this call built it. */
export type PrepareResult = Partial<Record<BrowserName, 'ready' | 'built'>>;

export interface LocalImageOptions {
  /** Daemon address. Default LOCALIMAGE_ADDR or `127.0.0.1:7777`; use `https://host:port` for TLS. */
  addr?: string | undefined;
  /** Default LOCALIMAGE_API_KEY. */
  apiKey?: string | undefined;
  /** Start the daemon on the first call if none is running. Default: on when LOCALIMAGE_AUTOSTART=1. */
  autoStart?: boolean | undefined;
  /** Environment for a daemon started by autoStart, for example `{ LOCALIMAGE_MAX_SESSIONS: '20' }`. */
  daemonEnv?: Record<string, string> | undefined;
}

export interface EnsureDaemonOptions {
  addr?: string | undefined;
  apiKey?: string | undefined;
  /** Environment for a daemon started here. It does not affect one that is already running. */
  env?: Record<string, string> | undefined;
  /** How long to wait for it to answer. Default 30000. */
  timeoutMs?: number | undefined;
  logFile?: string | undefined;
  signal?: AbortSignal | undefined;
}

export interface EnsureDaemonResult {
  /** False when a daemon was already running and was reused. */
  started: boolean;
  addr: string;
  /** Only when started here. */
  pid?: number | undefined;
  logFile?: string | undefined;
}

export interface RecordingInfo {
  replay_id: string;
  status: 'recording' | 'stopped';
  running: boolean;
  /** When capture began, the video's time zero. RFC 3339, UTC, millisecond precision. */
  started_at: string;
  started_at_ms: number;
  /** When capture was told to stop: the video's end. */
  stopped_at?: string | undefined;
  stopped_at_ms?: number | undefined;
  /** When the file was complete, a moment after stopped_at. */
  finalized_at?: string | undefined;
  finalized_at_ms?: number | undefined;
  /** Measured media duration, once stopped. */
  duration_ms?: number | undefined;
  size: number;
  content_type: 'video/mp4';
  format: 'mp4';
  codec?: string | undefined;
  pix_fmt?: string | undefined;
  width?: number | undefined;
  height?: number | undefined;
  fps?: number | undefined;
}

export interface ExecOptions extends CallOptions {
  cwd?: string | undefined;
  /** Limit for the command itself, in seconds (default 30, at most 300). */
  timeoutSeconds?: number | undefined;
}

export interface ExecResult {
  exit_code: number;
  stdout: string;
  stderr: string;
  /** Output was cut at 1 MiB per stream. */
  truncated?: boolean | undefined;
  /** The command was killed at its timeoutSeconds. */
  timed_out?: boolean | undefined;
}

export interface FileInfo {
  name: string;
  type: 'file' | 'dir' | 'link' | 'other';
  size: number;
  /** Unix seconds. */
  mtime: number;
}

export interface ExtensionInfo {
  name: string;
  title?: string | undefined;
  version?: string | undefined;
}

export interface ScreenshotRegion {
  x: number;
  y: number;
  width: number;
  height: number;
}

export type MouseButton = 'left' | 'middle' | 'right';

/** Which of a session's logs to read. `container` is Docker's capture of its output. */
export type LogSource = 'container' | 'chromium' | 'firefox' | 'neko' | 'xorg' | 'openbox' | 'pulseaudio' | 'cdp-forward';

/** Whether a session's browser is up, and whether it has restarted. */
export interface BrowserHealth {
  state: string;
  health?: 'starting' | 'healthy' | 'unhealthy' | undefined;
  /** How many times the browser has started (1 after a normal launch). */
  starts: number;
  /** `starts - 1`: how often it crashed and came back as a new, empty browser. */
  restarts: number;
  last_started_at?: string | undefined;
  last_started_at_ms?: number | undefined;
}
