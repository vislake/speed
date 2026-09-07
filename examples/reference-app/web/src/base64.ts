/**
 * base64.ts -- the binary-safe base64 decode the case page's image
 * rendering shares: the generated surface carries stored bytes as
 * base64 in JSON (the only transport this host's generated calls
 * speak), and atob alone hands back a binary string whose characters
 * the Blob constructor would mis-encode. The explicit
 * ArrayBuffer-backed type keeps the result a valid BlobPart under
 * TypeScript 5.9's typed-array generics.
 */

/** Decodes a base64 payload into its bytes (binary-safe: atob hands
 * back a binary string; the char-code loop keeps every byte). */
export function base64ToBytes(contentBase64: string): Uint8Array<ArrayBuffer> {
  const binary = atob(contentBase64)
  const bytes = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i++) {
    bytes[i] = binary.charCodeAt(i)
  }
  return bytes
}
