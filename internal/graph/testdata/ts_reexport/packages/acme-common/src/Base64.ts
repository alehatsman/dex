export function encode(s: string): string {
  return Buffer.from(s).toString('base64');
}
