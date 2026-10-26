// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest';
import { copyText } from './clipboard';

/** jsdom has no execCommand; this stands in for a browser's, recording what
 *  was selected when it copied. */
function execCommand(result: boolean | 'throws' = true) {
  const copied: string[] = [];
  const parents: (Element | null)[] = [];
  const fn = vi.fn((command: string) => {
    if (result === 'throws') throw new Error('refused');
    const active = document.activeElement;
    if (command === 'copy' && active instanceof HTMLTextAreaElement) {
      copied.push(active.value.slice(active.selectionStart, active.selectionEnd));
      parents.push(active.parentElement);
    }
    return result;
  });
  Object.defineProperty(document, 'execCommand', { value: fn, configurable: true });
  return { fn, copied, parents };
}

afterEach(() => {
  vi.unstubAllGlobals();
  Reflect.deleteProperty(document, 'execCommand');
  document.body.innerHTML = '';
});

describe('copyText', () => {
  it('writes through the clipboard API where there is one', async () => {
    const writeText = vi.fn(async () => {});
    vi.stubGlobal('navigator', { clipboard: { writeText } });
    const { fn } = execCommand();
    expect(await copyText('a value')).toBe(true);
    expect(writeText).toHaveBeenCalledWith('a value');
    expect(fn).not.toHaveBeenCalled();
  });

  it('copies from a selected field over plain HTTP, where the API is absent', async () => {
    vi.stubGlobal('navigator', {});
    const { copied } = execCommand();
    expect(await copyText('one\ntwo')).toBe(true);
    expect(copied).toEqual(['one\ntwo']);
    // The field is gone afterwards.
    expect(document.querySelector('textarea')).toBeNull();
  });

  it('falls back when the clipboard API refuses', async () => {
    vi.stubGlobal('navigator', { clipboard: { writeText: vi.fn(async () => Promise.reject(new Error('denied'))) } });
    const { copied } = execCommand();
    expect(await copyText('a value')).toBe(true);
    expect(copied).toEqual(['a value']);
  });

  it('says it did not copy when the browser refuses both', async () => {
    vi.stubGlobal('navigator', {});
    expect(await copyText('a value')).toBe(false);
    execCommand(false);
    expect(await copyText('a value')).toBe(false);
    execCommand('throws');
    expect(await copyText('a value')).toBe(false);
  });

  it('copies from inside a modal dialog, and gives focus back', async () => {
    vi.stubGlobal('navigator', {});
    const dialog = document.createElement('dialog');
    dialog.setAttribute('open', '');
    const button = document.createElement('button');
    const text = document.createElement('p');
    text.textContent = 'kept';
    dialog.append(text, button);
    document.body.append(dialog);
    button.focus();
    const range = document.createRange();
    range.selectNodeContents(text);
    document.getSelection()!.addRange(range);

    const { copied, parents } = execCommand();
    expect(await copyText('in a dialog')).toBe(true);
    expect(copied).toEqual(['in a dialog']);
    expect(parents).toEqual([dialog]);
    expect(document.activeElement).toBe(button);
    // The reader's own selection is put back too; jsdom collapses every range
    // when a field is selected, so this checks only that one is put back.
    expect(document.getSelection()!.rangeCount).toBe(1);
  });
});
