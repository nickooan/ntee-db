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
  put: def("nteedb_put", ["uint", "str", "void *", "int", "str"]),
  incr: def("nteedb_incr", ["uint", "str", "int64"]),
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

// callAsync runs a koffi function off the event loop (libuv thread) and resolves
// with its parsed result.
export function callAsync(fn, ...args) {
  return new Promise((resolve, reject) => {
    fn.async(...args, (err, s) => {
      if (err) return reject(err)
      try {
        resolve(readEnvelope(s))
      } catch (e) {
        reject(e)
      }
    })
  })
}
