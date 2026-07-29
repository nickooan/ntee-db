// Quick tour against a running nteedb-server. Start one first, e.g.:
//
//   go run ./nteedb-server -schema nteedb-server/schema.example.json -dir $(mktemp -d)
//
// then: node example.mjs
// (NTEEDB_HOST / NTEEDB_PORT / NTEEDB_AUTH / NTEEDB_TLS=1 to override;
// the port defaults to 6666, or 6667 with NTEEDB_TLS=1)
import { NteeClient } from "./src/index.js"

const client = await NteeClient.connect({
  host: process.env.NTEEDB_HOST ?? "127.0.0.1",
  port: process.env.NTEEDB_PORT ? Number(process.env.NTEEDB_PORT) : undefined,
  tls: process.env.NTEEDB_TLS === "1" ? true : undefined,
  auth: process.env.NTEEDB_AUTH,
})
console.log("connected:", client.info)

await client.put("user:1", { name: "Ada", role: "admin" })
console.log("get:", await client.get("user:1"))

await client.put("call:1", { kind: "request" }, { traceId: "T1" })
console.log("secIndex traceId=T1:", await client.secIndex("traceId", "T1"))

console.log("incr hits:", await client.incr("hits"))
console.log(
  "topup quota 10/100 → overflow:",
  await client.topup("quota", 10, 100),
)
console.log(
  "take quota 3, floor 0 → applied:",
  await client.take("quota", 3, 0),
)

console.log("stats:", await client.stats())
await client.close()
