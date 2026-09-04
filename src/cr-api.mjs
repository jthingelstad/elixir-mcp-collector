/**
 * The only code in the system that speaks to api.clashroyale.com
 * (AGENTS.md rule 2). Single attempt per lease: errors are posted as
 * results and the scheduler replans — retry policy lives in one place,
 * not two.
 */

const BASE = "https://api.clashroyale.com/v1";
const TIMEOUT_MS = 15_000;

const enc = (tag) => encodeURIComponent(tag);

const PATH_BY_ENDPOINT = {
  player: (key) => `/players/${enc(key)}`,
  player_battlelog: (key) => `/players/${enc(key)}/battlelog`,
  clan: (key) => `/clans/${enc(key)}`,
  currentriverrace: (key) => `/clans/${enc(key)}/currentriverrace`,
  riverracelog: (key) => `/clans/${enc(key)}/riverracelog`,
  cards: () => `/cards`, // global catalog; entity_key is the GLOBAL sentinel
};

export function crPath(job) {
  const build = PATH_BY_ENDPOINT[job.endpoint];
  if (!build) throw new Error(`no CR path for endpoint: ${job.endpoint}`);
  return build(job.entity_key);
}

export function makeCrFetch({ token, fetchImpl = fetch }) {
  return async function crFetch(path) {
    try {
      const res = await fetchImpl(`${BASE}${path}`, {
        headers: {
          authorization: `Bearer ${token}`,
          "user-agent": "Elixir-MCP-Gateway/0.1",
        },
        signal: AbortSignal.timeout(TIMEOUT_MS),
      });
      const bodyText = await res.text();
      return {
        kind: "http",
        status: res.status,
        bodyText,
        retryAfterSeconds: res.headers.get("retry-after")
          ? Number(res.headers.get("retry-after"))
          : null,
      };
    } catch (err) {
      return { kind: "transport", message: err?.message ?? "fetch failed" };
    }
  };
}
