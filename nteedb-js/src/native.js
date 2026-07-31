// Loads the platform-specific libnteedb shared library and declares the C ABI
// via koffi. Every C function returns a malloc'd JSON-envelope C-string; we use
// a koffi "disposable" return type so koffi decodes it to a JS string and frees
// the C memory automatically (via our nteedb_free).
import koffi from "koffi"
import path from "node:path"
import { fileURLToPath } from "node:url"

const here = path.dirname(fileURLToPath(import.meta.url))

function libraryPath() {
  if (process.platform === "win32") {
    throw new Error(
      "nteedb: Windows is not supported yet (no prebuilt library ships for it; " +
        "prebuilds cover darwin-arm64, linux-amd64, linux-arm64)",
    )
  }
  const osName = process.platform // darwin | linux
  const goarch = process.arch === "x64" ? "amd64" : process.arch // x64→amd64, arm64→arm64
  const ext = process.platform === "darwin" ? "dylib" : "so"
  return path.join(
    here,
    "..",
    "prebuilds",
    `${osName}-${goarch}`,
    `libnteedb.${ext}`,
  )
}

const lib = (() => {
  const path = libraryPath()
  try {
    return koffi.load(path)
  } catch (cause) {
    // A raw dlopen error is unactionable; say what was looked for and how to fix.
    throw new Error(
      `nteedb: no native library for ${process.platform}-${process.arch} at ${path} ` +
        `(prebuilds ship for darwin-arm64, linux-amd64, linux-arm64). ` +
        `To build one for this host, clone https://github.com/nickooan/ntee-db ` +
        `and run nteedb-js/capi/build.sh (requires Go), then copy the library ` +
        `into this package's prebuilds/ directory. Original error: ${cause.message}`,
      { cause },
    )
  }
})()

const free = lib.func("nteedb_free", "void", ["void *"])

// A NUL-terminated string that koffi decodes to a JS string and frees with our
// allocator after each call (no leaks, no manual decode). Anonymous (no name) so
// re-evaluating this module — e.g. across jest suites, which share koffi's
// process-global type registry — never throws "duplicate type name".
const Str = koffi.disposable("str", free)

const def = (name, args) => lib.func(name, Str, args)

export const fns = {
  open: def("nteedb_open", ["str", "str"]),
  close: def("nteedb_close", ["uint"]),
  drop: def("nteedb_drop", ["uint"]),
  destroy: def("nteedb_destroy", ["str"]),
  put: def("nteedb_put", ["uint", "str", "void *", "int", "str", "int64"]),
  incr: def("nteedb_incr", ["uint", "str", "int64", "int64"]),
  topup: def("nteedb_topup", ["uint", "str", "int64", "int64", "int64"]),
  take: def("nteedb_take", ["uint", "str", "int64", "int64", "int64"]),
  putBatch: def("nteedb_put_batch", ["uint", "str"]),
  putBatchBin: def("nteedb_put_batch_bin", ["uint", "str", "void *", "int"]),
  getJson: def("nteedb_get_json", ["uint", "str"]),
  getManyJson: def("nteedb_get_many_json", ["uint", "str"]),
  has: def("nteedb_has", ["uint", "str"]),
  stats: def("nteedb_stats", ["uint"]),
  delete: def("nteedb_delete", ["uint", "str"]),
  prefixScan: def("nteedb_prefix_scan", ["uint", "str"]),
  byIndex: def("nteedb_by_index", ["uint", "str", "str", "int"]),
  byIndexHas: def("nteedb_by_index_has", ["uint", "str", "str"]),
  byIndexPrefix: def("nteedb_by_index_prefix", ["uint", "str", "str", "int"]),
  byIndexRange: def("nteedb_by_index_range", ["uint", "str", "str", "str"]),
  byIndexRecordsJson: def("nteedb_by_index_records_json", [
    "uint",
    "str",
    "str",
    "int",
  ]),
  byIndexPrefixRecordsJson: def("nteedb_by_index_prefix_records_json", [
    "uint",
    "str",
    "str",
    "int",
  ]),
  prefixScanRecordsJson: def("nteedb_prefix_scan_records_json", [
    "uint",
    "str",
  ]),
  removeByPkLess: def("nteedb_remove_by_pk_less", ["uint", "str"]),
  removeByPkGreater: def("nteedb_remove_by_pk_greater", ["uint", "str"]),
  compact: def("nteedb_compact", ["uint"]),
  reindex: def("nteedb_reindex", ["uint"]),
  relieve: def("nteedb_relieve", ["uint"]),
  blobUsage: def("nteedb_blob_usage", ["uint"]),
  details: def("nteedb_details", ["uint"]),
  droppedIndexes: def("nteedb_dropped_indexes", ["uint"]),
  prospectiveIndexes: def("nteedb_prospective_indexes", ["uint"]),
}

// readEnvelope parses the JSON envelope string, returning `result` or throwing `err`.
export function readEnvelope(s) {
  if (typeof s !== "string") {
    throw new Error(`nteedb: native call returned no envelope (got ${s})`)
  }
  let env
  try {
    env = JSON.parse(s)
  } catch {
    throw new Error(
      `nteedb: malformed envelope from native library: ${s.slice(0, 120)}`,
    )
  }
  if (env === null) {
    throw new Error("nteedb: malformed envelope from native library: null")
  }
  if (env.err) throw new Error(env.err)
  return env.result
}

// koffi caps concurrent async calls at 256 per process and throws
// "Too many asynchronous calls are running" past it. Gate in-flight calls
// below that (headroom: koffi frees a slot only after the completion callback
// returns to C++, so completed-but-unfreed slots transiently coexist with
// newly launched ones) and FIFO-queue the rest, so unbounded Promise.all
// fan-outs never surface the cap. Module-level on purpose: the cap is
// process-global, shared by every NteeDB instance.
let maxInFlight = 200
let inFlight = 0
const waiters = [] // FIFO of resolvers awaiting a slot
let head = 0 // head pointer instead of O(n) shift()

// Test hook, not public API.
export const setMaxInFlight = (n) => {
  maxInFlight = n
}

const acquire = () => {
  if (inFlight < maxInFlight) {
    inFlight++
    return null // fast path: dispatch synchronously, no queue hop
  }
  return new Promise((resolve) => waiters.push(resolve))
}

const release = () => {
  if (head < waiters.length) {
    const next = waiters[head]
    waiters[head++] = undefined
    if (head === waiters.length) {
      waiters.length = 0 // drained: free the whole backing store
      head = 0
    } else if (head >= 1024 && head * 2 >= waiters.length) {
      // Never-drained queues (sustained saturation) would otherwise grow the
      // dead [0, head) prefix forever. Compact once it's big and ≥ half the
      // array: the copy moves ≤ live-count elements only after head advanced
      // that many times, so the cost stays amortized O(1) per waiter.
      waiters.copyWithin(0, head)
      waiters.length -= head
      head = 0
    }
    next() // hand the slot to the next waiter; inFlight unchanged
  } else {
    inFlight--
  }
}

const dispatch = (fn, args) =>
  new Promise((resolve, reject) => {
    try {
      fn.async(...args, (err, s) => {
        release() // first: even a readEnvelope throw must free the slot
        if (err) return reject(err)
        try {
          resolve(readEnvelope(s))
        } catch (e) {
          reject(e)
        }
      })
    } catch (e) {
      release() // fn.async threw synchronously; the call never launched
      throw e // executor throw → promise rejection
    }
  })

// callAsync runs a koffi function off the event loop (libuv thread) and resolves
// with its parsed result. Calls beyond the in-flight gate wait their turn as
// plain pending promises (no timers, nothing keeping the event loop alive).
export const callAsync = (fn, ...args) => {
  const gate = acquire()
  return gate ? gate.then(() => dispatch(fn, args)) : dispatch(fn, args)
}
