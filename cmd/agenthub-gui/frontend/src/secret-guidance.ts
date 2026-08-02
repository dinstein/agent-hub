/**
 * One-shot handoffs between the Servers and Secrets routes.
 *
 * They stay in memory: key NAMES are not credentials, but a setup intent is
 * still transient UI state rather than configuration. A reload simply falls
 * back to the normal Secrets page instead of replaying an old write prompt.
 */
export interface SecretSetupRequest {
  server: string;
  keys: string[];
  returnToServers: boolean;
}

let pendingSetup: SecretSetupRequest | null = null;
let pendingRetest = "";

export function openSecretSetup(server: string, keys: string[] = []): void {
  pendingSetup = {
    server,
    keys: keys.filter((key) => key.trim() !== ""),
    returnToServers: true,
  };
  window.location.hash = "#/secrets";
}

export function consumeSecretSetup(): SecretSetupRequest | null {
  const request = pendingSetup;
  pendingSetup = null;
  return request;
}

export function returnToServerTest(server: string): void {
  pendingRetest = server;
  window.location.hash = "#/servers";
}

export function consumeServerRetest(): string {
  const server = pendingRetest;
  pendingRetest = "";
  return server;
}
