// Packing files from somebody's computer into the archive Pando keeps as an
// app's source (R-262, design 04 §4).
//
// The same archive `pando deploy ./` sends — a gzipped tar, paths relative to
// the app's root, regular files only — so the server reads an upload from the
// console exactly as it reads one from the CLI. Built here rather than with a
// library: a tar is a 512-byte header per file, and the browser already ships
// gzip as CompressionStream.

/** One file, and where it sits relative to what was chosen. */
export interface PickedFile {
  path: string;
  file: Blob & { lastModified?: number };
}

/**
 * Directories never uploaded, at any depth. The CLI's list (internal/cli/
 * upload.go), for the same reason: they are rebuilt, not deployed, and
 * node_modules alone is usually larger than the limit.
 */
const SKIPPED = new Set([
  '.git',
  'node_modules',
  '.venv',
  'venv',
  '__pycache__',
  '.terraform',
  'vendor',
  '.DS_Store',
  '.idea',
  '.vscode',
]);

/**
 * The files to upload, with paths made relative to the app's root.
 *
 * A folder chosen whole arrives as `site/index.html`, `site/css/app.css`: the
 * folder is the app, so its own name is dropped. A file chosen on its own —
 * a single index.html — is already at the root.
 */
export function prepare(picked: readonly PickedFile[]): PickedFile[] {
  const cleaned: PickedFile[] = [];
  for (const p of picked) {
    const path = clean(p.path);
    if (path === null) continue;
    if (path.split('/').some((segment) => SKIPPED.has(segment))) continue;
    cleaned.push({ path, file: p.file });
  }
  const root = commonRoot(cleaned.map((p) => p.path));
  const out = root === '' ? cleaned : cleaned.map((p) => ({ ...p, path: p.path.slice(root.length + 1) }));

  // The same path twice — the same file dropped twice — is one file.
  const seen = new Set<string>();
  return out.filter((p) => (seen.has(p.path) ? false : (seen.add(p.path), true)));
}

/** The name of the folder chosen whole, or '' when files were chosen. */
export function folderOf(picked: readonly PickedFile[]): string {
  return commonRoot(picked.map((p) => clean(p.path)).filter((p): p is string => p !== null));
}

/** A path with separators normalized, or null for one that leaves the root. */
function clean(raw: string): string | null {
  const parts = raw.replace(/\\/g, '/').split('/').filter((s) => s !== '' && s !== '.');
  if (parts.length === 0 || parts.includes('..')) return null;
  return parts.join('/');
}

/** The folder every path is inside, when there is exactly one. */
function commonRoot(paths: readonly string[]): string {
  if (paths.length === 0) return '';
  const first = paths[0]!.split('/');
  if (first.length < 2) return '';
  const root = first[0]!;
  return paths.every((p) => p.startsWith(root + '/')) ? root : '';
}

/** Total size of the files, in bytes. */
export function totalBytes(files: readonly PickedFile[]): number {
  return files.reduce((n, f) => n + f.file.size, 0);
}

/** A gzipped tar of the files. */
export async function pack(files: readonly PickedFile[]): Promise<Blob> {
  const chunks: Uint8Array[] = [];
  for (const f of files) {
    const body = new Uint8Array(await f.file.arrayBuffer());
    const mtime = Math.floor((f.file.lastModified ?? Date.now()) / 1000);
    if (utf8(f.path).length > 100) {
      // A long path goes in a PAX record ahead of the file, which every tar
      // reader since 2001 understands — Go's included.
      const record = paxRecord('path', f.path);
      chunks.push(header(`PaxHeader/${f.path.slice(-80)}`, record.length, mtime, 'x'), record, padding(record.length));
    }
    chunks.push(header(f.path, body.length, mtime, '0'), body, padding(body.length));
  }
  chunks.push(new Uint8Array(1024)); // two empty blocks end the archive

  const tar = new Blob(chunks as BlobPart[]);
  const gzipped = tar.stream().pipeThrough(new CompressionStream('gzip'));
  return new Blob([await new Response(gzipped).arrayBuffer()], { type: 'application/gzip' });
}

const encoder = new TextEncoder();
const utf8 = (s: string) => encoder.encode(s);

function padding(size: number): Uint8Array {
  const rest = size % 512;
  return new Uint8Array(rest === 0 ? 0 : 512 - rest);
}

/** "<length> key=value\n", where the length counts itself. */
function paxRecord(key: string, value: string): Uint8Array {
  const body = ` ${key}=${value}\n`;
  let length = utf8(body).length;
  for (;;) {
    const total = String(length).length + utf8(body).length;
    if (total === length) break;
    length = total;
  }
  return utf8(`${length}${body}`);
}

/** A ustar header block. */
function header(name: string, size: number, mtime: number, type: '0' | 'x'): Uint8Array {
  const block = new Uint8Array(512);
  const put = (offset: number, width: number, value: string) => {
    block.set(utf8(value).slice(0, width), offset);
  };
  const octal = (offset: number, width: number, value: number) => {
    put(offset, width, value.toString(8).padStart(width - 1, '0') + '\0');
  };

  put(0, 100, name);
  octal(100, 8, 0o644);
  octal(108, 8, 0);
  octal(116, 8, 0);
  octal(124, 12, size);
  octal(136, 12, mtime);
  put(148, 8, '        '); // the checksum counts its own field as spaces
  put(156, 1, type);
  put(257, 6, 'ustar\0');
  put(263, 2, '00');

  let sum = 0;
  for (const byte of block) sum += byte;
  put(148, 8, sum.toString(8).padStart(6, '0') + '\0 ');
  return block;
}

/**
 * Every file under a dropped item, with its path. A dropped folder is read
 * through the entries API, which is the only way a drop can see inside one.
 */
export async function filesFromDrop(items: DataTransferItemList): Promise<PickedFile[]> {
  const entries: FileSystemEntry[] = [];
  for (const item of Array.from(items)) {
    const entry = item.webkitGetAsEntry?.();
    if (entry) entries.push(entry);
  }
  const out: PickedFile[] = [];
  for (const entry of entries) await walk(entry, '', out);
  return out;
}

async function walk(entry: FileSystemEntry, prefix: string, out: PickedFile[]): Promise<void> {
  const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
  if (entry.isFile) {
    const file = await new Promise<File>((resolve, reject) => (entry as FileSystemFileEntry).file(resolve, reject));
    out.push({ path, file });
    return;
  }
  if (!entry.isDirectory || SKIPPED.has(entry.name)) return;
  const reader = (entry as FileSystemDirectoryEntry).createReader();
  // readEntries answers in batches until it answers with none.
  for (;;) {
    const batch = await new Promise<FileSystemEntry[]>((resolve, reject) => reader.readEntries(resolve, reject));
    if (batch.length === 0) break;
    for (const child of batch) await walk(child, path, out);
  }
}

/** Files from an <input type="file">, folder or not. */
export function filesFromInput(list: FileList | null): PickedFile[] {
  return Array.from(list ?? []).map((file) => ({
    path: (file as File & { webkitRelativePath?: string }).webkitRelativePath || file.name,
    file,
  }));
}
