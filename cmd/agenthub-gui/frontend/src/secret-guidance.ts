/** One-shot handoff from another route to one Server's secret manager. */
export interface ServerSecretRequest {
  server: string;
  keys: string[];
}

let pending: ServerSecretRequest | null = null;

export function requestServerSecrets(server: string, keys: string[] = []): void {
  pending = {
    server,
    keys: keys.filter((key) => key.trim() !== ""),
  };
  window.location.hash = "#/servers";
}

export function consumeServerSecrets(): ServerSecretRequest | null {
  const request = pending;
  pending = null;
  return request;
}
