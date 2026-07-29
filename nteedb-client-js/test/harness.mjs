// Test harness: builds the real nteedb-server once per run (Go toolchain
// required), then spawns one server per fixture on an ephemeral port with a
// fresh store directory (the store is single-writer via flock).
import { spawn, execFile } from "node:child_process"
import { mkdtemp, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import path from "node:path"
import { fileURLToPath } from "node:url"
import { NteeClient } from "../src/index.js"

const HERE = path.dirname(fileURLToPath(import.meta.url))
const REPO_ROOT = path.resolve(HERE, "..", "..")

// Test-only self-signed pair (see certs/README.md).
export const TLS_CERT = path.join(HERE, "certs", "server.crt")
export const TLS_KEY = path.join(HERE, "certs", "server.key")

export const DEFAULT_INDEXES = [
  { name: "traceId", kind: "string" },
  { name: "status", kind: "number" },
  { name: "kind", kind: "string", jsonPath: "kind" },
]

let buildPromise = null

/** Build ./nteedb-server into a shared temp dir, once per process. */
export function buildServer() {
  buildPromise ??= (async () => {
    const dir = await mkdtemp(path.join(tmpdir(), "nteedb-server-bin-"))
    const bin = path.join(dir, "nteedb-server")
    await new Promise((resolve, reject) => {
      execFile(
        "go",
        ["build", "-o", bin, "./nteedb-server"],
        { cwd: REPO_ROOT },
        (err, _stdout, stderr) => {
          if (err) {
            reject(
              new Error(
                `building nteedb-server failed (a Go toolchain is required to run these tests): ${stderr || err.message}`,
              ),
            )
          } else {
            resolve()
          }
        },
      )
    })
    process.on("exit", () => {}) // dir is in tmp; leave cleanup to the OS
    return bin
  })()
  return buildPromise
}

/**
 * Start a server on 127.0.0.1:0 with a fresh store.
 * opts: { indexes?, auth? (password string), authFile? (file contents),
 * tls? (true → also start a TLS listener on an ephemeral port using the
 * committed test cert) }.
 * Returns { host, port, tlsPort?, stop } — stop() terminates the server and
 * removes the scratch dir.
 */
export async function startServer(opts = {}) {
  const bin = await buildServer()
  const dir = await mkdtemp(path.join(tmpdir(), "nteedb-client-"))
  const schemaPath = path.join(dir, "schema.json")
  await writeFile(
    schemaPath,
    JSON.stringify({
      dir: path.join(dir, "store"),
      indexes: opts.indexes ?? DEFAULT_INDEXES,
    }),
  )
  const args = ["-schema", schemaPath, "-addr", "127.0.0.1:0"]
  if (opts.tls) {
    args.push(
      "-tls-addr",
      "127.0.0.1:0",
      "-tls-cert",
      TLS_CERT,
      "-tls-key",
      TLS_KEY,
    )
  }
  if (opts.auth) args.push("-auth", opts.auth)
  if (opts.authFile) {
    const authPath = path.join(dir, "users.txt")
    await writeFile(authPath, opts.authFile)
    args.push("-auth-file", authPath)
  }
  const child = spawn(bin, args, { stdio: ["ignore", "ignore", "pipe"] })

  let stderrBuf = ""
  const { addr, tlsAddr } = await new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      child.kill("SIGKILL")
      reject(
        new Error(`server did not report an address; stderr:\n${stderrBuf}`),
      )
    }, 10_000)
    child.stderr.setEncoding("utf8")
    child.stderr.on("data", (chunk) => {
      stderrBuf += chunk
      const m = stderrBuf.match(/listening on (\S+)/)
      if (!m) return
      // With TLS the server logs a second line ("tls on <addr>") right
      // after the plain one; wait for it so both ports are known.
      const mt = opts.tls ? stderrBuf.match(/tls on (\S+)/) : null
      if (opts.tls && !mt) return
      clearTimeout(timer)
      resolve({ addr: m[1], tlsAddr: mt?.[1] })
    })
    child.once("exit", (code) => {
      clearTimeout(timer)
      reject(
        new Error(`server exited early (code ${code}); stderr:\n${stderrBuf}`),
      )
    })
  })
  const portOf = (a) => Number(a.slice(a.lastIndexOf(":") + 1))
  const host = addr.slice(0, addr.lastIndexOf(":"))
  const port = portOf(addr)
  const tlsPort = tlsAddr ? portOf(tlsAddr) : undefined

  let stopped = false
  async function stop() {
    if (stopped) return
    stopped = true
    // The child may already be gone (a test killed it); 'exit' won't fire
    // again, so only wait when it is still running.
    if (child.exitCode === null && child.signalCode === null) {
      await new Promise((resolve) => {
        child.once("exit", resolve)
        child.kill("SIGTERM")
        setTimeout(() => child.kill("SIGKILL"), 3000).unref()
      })
    }
    await rm(dir, { recursive: true, force: true })
  }
  return { host, port, tlsPort, stop, child }
}

/** Run fn against a fresh server, always stopping it afterwards. */
export async function withServer(opts, fn) {
  const srv = await startServer(opts)
  try {
    return await fn(srv)
  } finally {
    await srv.stop()
  }
}

/** Run fn with a connected client against a fresh server. */
export async function withClient(serverOpts, clientOpts, fn) {
  return withServer(serverOpts, async (srv) => {
    const client = await NteeClient.connect({
      host: srv.host,
      port: srv.port,
      ...clientOpts,
    })
    try {
      return await fn(client, srv)
    } finally {
      await client.close()
    }
  })
}
