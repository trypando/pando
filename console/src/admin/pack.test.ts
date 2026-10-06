import { describe, expect, it } from 'vitest';

import { folderOf, pack, prepare, totalBytes, type PickedFile } from './pack';

const file = (path: string, text: string): PickedFile => ({ path, file: new Blob([text]) });

describe('prepare', () => {
  it('keeps a single file at the root', () => {
    expect(prepare([file('index.html', '<h1>hi</h1>')]).map((f) => f.path)).toEqual(['index.html']);
  });

  it('drops the name of a folder chosen whole, because the folder is the app', () => {
    const picked = [file('site/index.html', 'a'), file('site/css/app.css', 'b')];
    expect(prepare(picked).map((f) => f.path)).toEqual(['index.html', 'css/app.css']);
    expect(folderOf(picked)).toBe('site');
    expect(folderOf([file('index.html', 'a')])).toBe('');
  });

  it('leaves out what is rebuilt rather than deployed, at any depth', () => {
    const got = prepare([
      file('app/package.json', '{}'),
      file('app/node_modules/left-pad/index.js', ''),
      file('app/.git/HEAD', ''),
      file('app/web/__pycache__/x.pyc', ''),
    ]);
    expect(got.map((f) => f.path)).toEqual(['package.json']);
  });

  it('refuses a path that leaves the root, and counts a file dropped twice once', () => {
    const got = prepare([file('../etc/passwd', 'x'), file('a.txt', '1'), file('./a.txt', '1')]);
    expect(got.map((f) => f.path)).toEqual(['a.txt']);
    expect(totalBytes(got)).toBe(1);
  });
});

/** Reads a gzipped tar back: each file's path, honoring PAX paths, and text. */
async function unpack(archive: Blob): Promise<Record<string, string>> {
  const raw = new Uint8Array(await new Response(archive.stream().pipeThrough(new DecompressionStream('gzip'))).arrayBuffer());
  const text = new TextDecoder();
  const field = (block: Uint8Array, at: number, width: number) =>
    text.decode(block.subarray(at, at + width)).replace(/\0.*$/s, '');

  const out: Record<string, string> = {};
  let pax: string | null = null;
  for (let at = 0; at + 512 <= raw.length; ) {
    const block = raw.subarray(at, at + 512);
    if (block.every((b) => b === 0)) break;

    // The checksum, as a reader checks it: the header's bytes summed with the
    // checksum field read as spaces.
    let sum = 0;
    block.forEach((b, i) => (sum += i >= 148 && i < 156 ? 32 : b));
    expect(parseInt(field(block, 148, 8).trim(), 8)).toBe(sum);
    expect(field(block, 257, 6)).toBe('ustar');

    const size = parseInt(field(block, 124, 12), 8);
    const body = raw.subarray(at + 512, at + 512 + size);
    const type = field(block, 156, 1);
    if (type === 'x') {
      pax = /\d+ path=(.*)\n/.exec(text.decode(body))?.[1] ?? null;
    } else {
      out[pax ?? field(block, 0, 100)] = text.decode(body);
      pax = null;
    }
    at += 512 + Math.ceil(size / 512) * 512;
  }
  return out;
}

describe('pack', () => {
  it('produces a gzipped ustar archive, long paths carried in PAX records', async () => {
    const long = `${'deep/'.repeat(30)}page.html`;
    const archive = await pack(prepare([file('index.html', '<h1>hello</h1>'), file(long, 'deep')]));
    expect(archive.type).toBe('application/gzip');
    expect(await unpack(archive)).toEqual({ 'index.html': '<h1>hello</h1>', [long]: 'deep' });
  });
});
