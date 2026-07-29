// Pure helpers for the nteedb-server wire protocol: value normalization,
// argument validation, response-value decoding, and the length-prefixed
// write frames. No I/O here — everything is unit-testable without a server.

const CRLF = Buffer.from("\r\n")

/**
 * Normalize a user value for storage: a string or Buffer passes through
 * verbatim; undefined is a programming error; everything else (object,
 * array, number, boolean, null) is JSON-serialized. Mirrors the embedded
 * binding's toStorable so the two packages accept the same inputs.
 */
export function toStorable(value) {
  if (value === undefined) {
    throw new TypeError(
      "nteedb: value is undefined (use null for an intentional JSON null)",
    )
  }
  if (typeof value === "string" || Buffer.isBuffer(value)) return value
  return JSON.stringify(value)
}

/**
 * Validate a protocol token (key, index name, string index value, cutoff,
 * username, password). The server splits command lines on whitespace with no
 * quoting or escaping, so a token containing whitespace is not expressible —
 * reject it here before it silently corrupts the command line.
 */
export function assertToken(name, token) {
  if (typeof token !== "string" || token.length === 0) {
    throw new TypeError(`nteedb: ${name} must be a non-empty string`)
  }
  if (/\s/.test(token)) {
    throw new TypeError(
      `nteedb: ${name} must not contain whitespace (the protocol has no quoting): ${JSON.stringify(token)}`,
    )
  }
}

/**
 * Decode one value from a get/getm/ixrec response. The server embeds stored
 * bytes that are valid JSON verbatim (so they arrive parsed), and wraps
 * anything else — including plain text — as {"bin":true,"base64":"…"}.
 * Exactly that shape (both keys, no others) decodes to a Buffer; everything
 * else passes through untouched. A stored literal {bin:true, base64:"…"}
 * object is indistinguishable on the wire and also decodes to a Buffer —
 * a documented protocol-level ambiguity.
 */
export function decodeValue(parsed) {
  if (
    parsed !== null &&
    typeof parsed === "object" &&
    !Array.isArray(parsed) &&
    parsed.bin === true &&
    typeof parsed.base64 === "string" &&
    Object.keys(parsed).length === 2
  ) {
    return Buffer.from(parsed.base64, "base64")
  }
  return parsed
}

/**
 * Build the length-prefixed put frame:
 *
 *   put <key> <nbytes>\r\n
 *   <nbytes raw bytes>\r\n
 *
 * The client always uses this form — the inline `put <key> <rest>` sugar is
 * a trap for programmatic use (a value of "42" would be read as a byte
 * count, empty values are inexpressible, and the value would count against
 * the 1 MiB line limit).
 */
export function buildPut(key, valueBuf) {
  return Buffer.concat([
    Buffer.from(`put ${key} ${valueBuf.length}\r\n`),
    valueBuf,
    CRLF,
  ])
}

/**
 * Build the putx frame (indexed put — always length-prefixed):
 *
 *   putx <key> <ixbytes> <nbytes> [ttlms]\r\n
 *   <index-values JSON object>\r\n
 *   <value bytes>\r\n
 */
export function buildPutx(key, ixBuf, valueBuf, ttlMs = 0) {
  const ttl = ttlMs > 0 ? ` ${ttlMs}` : ""
  return Buffer.concat([
    Buffer.from(`putx ${key} ${ixBuf.length} ${valueBuf.length}${ttl}\r\n`),
    ixBuf,
    CRLF,
    valueBuf,
    CRLF,
  ])
}

/**
 * Build the putex frame — the framed put with a time-to-live (a trailing TTL
 * on plain `put` would be ambiguous with its inline form on the wire):
 *
 *   putex <key> <ttlms> <nbytes>\r\n
 *   <value bytes>\r\n
 */
export function buildPutex(key, ttlMs, valueBuf) {
  return Buffer.concat([
    Buffer.from(`putex ${key} ${ttlMs} ${valueBuf.length}\r\n`),
    valueBuf,
    CRLF,
  ])
}

/**
 * Normalize an optional ttlMs argument: undefined → 0 (no TTL); anything
 * else must be a positive safe integer number of milliseconds.
 */
export function ttlArg(ttlMs) {
  if (ttlMs === undefined) return 0
  if (!Number.isSafeInteger(ttlMs) || ttlMs <= 0) {
    throw new TypeError("nteedb: ttlMs must be a positive safe integer")
  }
  return ttlMs
}
