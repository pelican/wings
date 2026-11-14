// Reference Wings external plugin (Bun + TypeScript). Subscribes to server
// state changes and, when a server stops, asks Wings for the last console lines
// through a host callback. Speaks the Wings JSON-RPC 2.0 wire protocol over
// stdio (see plugin/sdk/SPEC.md). Bun executes this file directly.
//
//   bun index.ts         # how Wings launches it (with WINGS_PLUGIN=1 set)
//
// Anything written to stderr is logged by Wings under this plugin's name, so
// that is where this plugin writes its output.

type Id = number | string;

interface Message {
  jsonrpc?: string;
  id?: Id | null;
  method?: string;
  params?: any;
  result?: any;
  error?: { code: number; message: string };
}

interface WingsEvent {
  type: string;
  server: string;
  data?: Record<string, unknown>;
}

function send(msg: Message): void {
  process.stdout.write(JSON.stringify({ jsonrpc: "2.0", ...msg }) + "\n");
}

const log = (line: string): void => void process.stderr.write(line + "\n");

// Calls this plugin makes into Wings (host.*), matched to responses by id.
let nextId = 1;
const pending = new Map<Id, (m: Message) => void>();

function callHost(method: string, params: unknown): Promise<any> {
  const id = `p${nextId++}`;
  return new Promise((resolve, reject) => {
    pending.set(id, (m) => (m.error ? reject(new Error(m.error.message)) : resolve(m.result)));
    send({ id, method, params });
  });
}

async function onEvent(e: WingsEvent): Promise<void> {
  const state = String(e.data?.state ?? "");
  log(`server ${e.server} is now ${state}`);
  if (state !== "offline") return;

  // Requires the "server.logs" grant (requires.hostAPI in plugin.yaml).
  const res = await callHost("host.server.logs", { server: e.server, lines: 20 });
  for (const line of res?.lines ?? []) log(`  ${e.server} | ${line}`);
}

function handle(msg: Message): void {
  // A response to one of our host calls.
  if (msg.method === undefined && msg.id != null) {
    pending.get(msg.id)?.(msg);
    pending.delete(msg.id);
    return;
  }

  switch (msg.method) {
    case "initialize":
      send({
        id: msg.id,
        result: { name: "state-log", capability: "events", provides: { events: ["server.state"] } },
      });
      return;
    case "event": // a notification: no id, no reply
      onEvent(msg.params as WingsEvent).catch((err) => log(`event failed: ${err.message ?? err}`));
      return;
    default:
      if (msg.id != null) send({ id: msg.id, error: { code: -32601, message: "method not found" } });
  }
}

// Bun yields stdin one line at a time. When Wings closes stdin the loop ends
// and the process exits.
for await (const line of console) {
  const text = line.trim();
  if (!text) continue;
  try {
    handle(JSON.parse(text) as Message);
  } catch {
    // ignore unparseable frames
  }
}
