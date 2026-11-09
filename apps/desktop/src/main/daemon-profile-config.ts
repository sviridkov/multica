import { execFile } from "node:child_process";
import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";

interface DesktopProfileConfig {
  server_url?: string;
  token?: string;
}
interface Snapshot {
  path: string;
  revision: string;
  original: DesktopProfileConfig;
}
const snapshots = new WeakMap<DesktopProfileConfig, Snapshot>();
const ownedKeys = ["server_url", "token"] as const;

export async function readDesktopProfileConfig(path: string): Promise<DesktopProfileConfig> {
  let bytes: Buffer | undefined;
  try {
    bytes = await readFile(path);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
  }
  let parsed: unknown;
  try {
    parsed = bytes ? JSON.parse(bytes.toString("utf8")) : {};
  } catch {
    // JSON.parse diagnostics can include fragments of credentials.
    throw new Error("Invalid profile configuration; file was not changed");
  }
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw new Error("Profile configuration must be a JSON object");
  }
  const config: DesktopProfileConfig = {};
  for (const key of ownedKeys) {
    const value = (parsed as Record<string, unknown>)[key];
    if (value !== undefined && value !== null) {
      if (typeof value !== "string") throw new Error(`Invalid profile ${key}`);
      config[key] = value;
    }
  }
  snapshots.set(config, {
    path,
    revision: bytes ? createHash("sha256").update(bytes).digest("hex") : "missing",
    original: { ...config },
  });
  return config;
}

export async function writeDesktopProfileConfig(
  binary: string,
  profile: string,
  path: string,
  config: DesktopProfileConfig,
  options: { cwd?: string; env?: NodeJS.ProcessEnv } = {},
): Promise<void> {
  const snapshot = snapshots.get(config);
  if (!profile || !snapshot || snapshot.path !== path) {
    throw new Error("Profile write requires a resolved profile and its loaded snapshot");
  }
  const changes: Record<string, string | null> = {};
  for (const key of ownedKeys) {
    if (config[key] !== snapshot.original[key]) changes[key] = config[key] ?? null;
  }
  // Even a no-op is checked in Go: sidecar/auth decisions may depend on it.
  await new Promise<void>((resolve, reject) => {
    const child = execFile(
      binary,
      ["--profile", profile, "config", "patch-auth"],
      { ...options, timeout: 15_000, maxBuffer: 64 * 1024 },
      (error) => {
        if (error) {
          // Never expose stderr/argv from an incompatible or failing binary.
          reject(new Error("Profile save failed; reload and retry with a compatible CLI"));
        } else resolve();
      },
    );
    child.stdin?.on("error", () => {
      // execFile's callback reports process failure without echoing input.
    });
    child.stdin?.end(JSON.stringify({ expected_revision: snapshot.revision, changes }));
  });
  // A saved snapshot cannot be replayed. Every later edit must read again.
  snapshots.delete(config);
}
