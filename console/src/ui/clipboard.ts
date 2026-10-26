// Every copy button in the console writes through copyText (#322).
//
// navigator.clipboard exists only in a secure context, and Pando reached over
// plain HTTP on a private network, as the shipped compose file serves it, is
// not one. There the API is simply absent, so a button that only used it did
// nothing. The older route, execCommand('copy') on a selected field, still
// works over HTTP inside a click, and every current browser supports it.

/** Copies text to the clipboard, and says whether it did. Call it from the
 *  click itself: the fallback needs the click's user activation, which is why
 *  it runs before anything is awaited when the clipboard API is absent. */
export async function copyText(text: string): Promise<boolean> {
  if (!navigator.clipboard) return copyBySelection(text);
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // Refused — a permissions policy, or a document without focus. The
    // selection route can still succeed inside the same click.
    return copyBySelection(text);
  }
}

function copyBySelection(text: string): boolean {
  if (typeof document.execCommand !== 'function') return false;
  const before = document.activeElement instanceof HTMLElement ? document.activeElement : null;
  // Inside a modal dialog everything outside it is inert and cannot be
  // selected, so the field goes where the click came from.
  const host = before?.closest('dialog') ?? document.body;
  const field = document.createElement('textarea');
  field.value = text;
  field.readOnly = true;
  field.setAttribute('aria-hidden', 'true');
  Object.assign(field.style, { position: 'fixed', top: '0', left: '0', opacity: '0', pointerEvents: 'none' });
  host.appendChild(field);
  const selection = document.getSelection();
  const kept = selection && selection.rangeCount > 0 ? selection.getRangeAt(0).cloneRange() : null;
  field.focus({ preventScroll: true });
  field.select();
  let copied = false;
  try {
    // Deprecated, and the only clipboard write a page has outside a secure
    // context.
    copied = document.execCommand('copy');
  } catch {
    copied = false;
  }
  field.remove();
  before?.focus({ preventScroll: true });
  if (kept && selection) {
    selection.removeAllRanges();
    selection.addRange(kept);
  }
  return copied;
}
