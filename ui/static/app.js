// calmerge review UI: the few behaviors htmx and hx-live don't cover. Kept
// tiny on purpose; everything else is server-rendered.
(() => {
  const reduced = () => matchMedia('(prefers-reduced-motion: reduce)').matches;
  const desktop = () => matchMedia('(min-width: 1024px)').matches;

  // Resolve after el's animation ends, or after ms (so nothing ever hangs).
  const animated = (el, ms) => new Promise((done) => {
    const t = setTimeout(done, ms);
    el.addEventListener('animationend', () => { clearTimeout(t); done(); }, { once: true });
  });
  const wait = (ms) => new Promise((done) => setTimeout(done, ms));

  // ---- width morph ---------------------------------------------------------
  // [data-flip-width] elements (the status pill) animate from their old width
  // to their new one whenever their content changes. ResizeObserver runs after
  // layout but before paint, so the jump is never seen. Only a content swap
  // animates: resizes CSS already animates (the hover refresh icon sliding
  // in) would otherwise get frozen mid-way and snap when the flip lets go.
  const flip = new ResizeObserver((entries) => {
    for (const { target: el } of entries) {
      const w = el.getBoundingClientRect().width;
      const last = el.cmW;
      const text = el.textContent;
      const swapped = el.cmText != null && el.cmText !== text;
      el.cmW = w;
      el.cmText = text;
      if (last == null || !swapped || el.cmBusy || Math.abs(last - w) < 1 || reduced()) continue;
      el.cmBusy = true;
      el.style.width = `${last}px`;
      el.getBoundingClientRect(); // commit the old width before animating
      el.style.transition = 'width .4s cubic-bezier(.2, .9, .25, 1.05), background-color .3s ease, border-color .3s ease, color .3s ease';
      el.style.width = `${w}px`;
      wait(430).then(() => {
        el.style.width = el.style.transition = '';
        el.cmBusy = false;
        el.cmW = el.getBoundingClientRect().width;
      });
    }
  });
  const watch = () => document.querySelectorAll('[data-flip-width]').forEach((el) => {
    if (!el.cmWatched) { el.cmWatched = true; flip.observe(el); }
  });

  // ---- bottom sheets ------------------------------------------------------
  // Phone sheets (meeting details, the client picker) share one gesture model:
  // a handle you tap to toggle full height, or drag. On release the sheet
  // settles at full, normal or dismissed, by position and flick speed.

  const FULL_GAP = 12;    // px left above a full-height sheet
  const DISMISS_PX = 110; // drag this far below normal to close...
  const FLICK = 0.55;     // ...or flick faster than this (px/ms)

  // Only transforms move while animating (GPU, no re-layout per frame): at the
  // start of a gesture the sheet takes full height once, pushed down so it
  // looks unchanged; every motion is a translateY; a sheet that settles at
  // normal height gets its real height back afterwards, off-screen-invisible.
  const fullH = () => innerHeight - FULL_GAP;
  const backdropOf = (sheet) => sheet.parentElement.querySelector(':scope > .cm-backdrop, :scope > [data-backdrop]');
  const visibleH = (sheet) => innerHeight - sheet.getBoundingClientRect().top;
  const setY = (sheet, y) => { sheet.style.transform = `translate3d(0, ${y}px, 0)`; };

  // prepare: full height, offset to keep the current look. Returns the offset.
  const prepare = (sheet) => {
    if (sheet.cmNormal == null) sheet.cmNormal = sheet.getBoundingClientRect().height;
    const y = fullH() - visibleH(sheet);
    sheet.classList.remove('cm-snapping');
    sheet.style.maxHeight = 'none';
    sheet.style.height = `${fullH()}px`;
    setY(sheet, y);
    sheet.getBoundingClientRect(); // lay out once, now, not mid-animation
    return y;
  };

  const settle = (sheet, target) => {
    const bd = backdropOf(sheet);
    if (bd) { bd.style.transition = 'opacity .3s ease'; bd.style.opacity = ''; }
    sheet.classList.add('cm-snapping');
    const full = target === 'full';
    setY(sheet, full ? 0 : fullH() - sheet.cmNormal);
    sheet.dataset.snap = target;
    const token = (sheet.cmToken = (sheet.cmToken || 0) + 1);
    wait(reduced() ? 0 : 480).then(() => {
      if (token !== sheet.cmToken) return; // a newer gesture took over
      sheet.classList.remove('cm-snapping');
      if (!full) { // give back the real height; visually identical
        sheet.style.height = `${sheet.cmNormal}px`;
        setY(sheet, 0);
      }
    });
  };

  // dismiss slides the sheet away, then closes whatever owns it.
  const dismiss = async (sheet) => {
    sheet.cmToken = (sheet.cmToken || 0) + 1;
    sheet.classList.add('cm-snapping');
    setY(sheet, fullH() + 40);
    const bd = backdropOf(sheet);
    if (bd) { bd.style.transition = 'opacity .3s ease'; bd.style.opacity = '0'; }
    if (!reduced()) await wait(400);
    const menu = sheet.closest('details');
    if (menu) {
      menu.removeAttribute('open');
      sheet.classList.remove('cm-snapping');
      sheet.removeAttribute('style');
      delete sheet.dataset.snap;
      delete sheet.cmNormal;
      if (bd) bd.removeAttribute('style');
      return;
    }
    const close = sheet.querySelector('[data-close]');
    if (close) { cm.dir('select'); close.dispatchEvent(new CustomEvent('cm-close')); }
  };

  const toggleFull = (sheet) => {
    prepare(sheet);
    requestAnimationFrame(() => settle(sheet, sheet.dataset.snap === 'full' ? 'normal' : 'full'));
  };

  let drag = null;
  document.addEventListener('pointerdown', (e) => {
    const grab = e.target.closest('[data-grab]');
    if (!grab || desktop()) return;
    const sheet = grab.closest('[data-sheet]');
    if (!sheet) return;
    e.preventDefault(); // no text selection or focus ring while dragging
    drag = { sheet, grab, id: e.pointerId, y0: e.clientY, off0: null, last: [e.clientY, e.timeStamp], v: 0, moved: false };
    grab.setPointerCapture(e.pointerId);
  });

  document.addEventListener('pointermove', (e) => {
    if (!drag) return;
    const dy = e.clientY - drag.y0;
    if (!drag.moved && Math.abs(dy) <= 6) return;
    if (!drag.moved) { drag.moved = true; drag.off0 = prepare(drag.sheet); drag.sheet.cmToken = (drag.sheet.cmToken || 0) + 1; }
    const dt = e.timeStamp - drag.last[1];
    if (dt > 0) drag.v = 0.8 * ((e.clientY - drag.last[0]) / dt) + 0.2 * drag.v;
    drag.last = [e.clientY, e.timeStamp];
    const { sheet } = drag;
    let y = drag.off0 + dy;
    if (y < 0) y *= 0.2; // rubber-band past full
    setY(sheet, y);
    const bd = backdropOf(sheet);
    const below = y - (fullH() - sheet.cmNormal); // how far under normal height
    if (bd) { bd.style.transition = 'none'; bd.style.opacity = String(below > 0 ? Math.max(0, 1 - below / 320) : 1); }
  });

  const release = () => {
    if (!drag) return;
    const { sheet, moved, v } = drag;
    drag = null;
    if (!moved) return toggleFull(sheet); // a tap
    const vis = visibleH(sheet);
    const normal = sheet.cmNormal;
    if (vis < normal - DISMISS_PX || v > FLICK) return dismiss(sheet);
    if (v < -FLICK || vis > (normal + fullH()) / 2) return settle(sheet, 'full');
    return settle(sheet, 'normal');
  };
  document.addEventListener('pointerup', release);
  document.addEventListener('pointercancel', release);

  document.addEventListener('keydown', (e) => {
    // Enter/Space on a handle toggles full height, like a tap.
    const grab = e.target.closest?.('[data-grab]');
    if (grab && (e.key === 'Enter' || e.key === ' ')) {
      e.preventDefault();
      const sheet = grab.closest('[data-sheet]');
      if (sheet) toggleFull(sheet);
      return;
    }
    // Escape closes the details panel.
    if (e.key === 'Escape') {
      const close = document.querySelector('.cm-sheet [data-close]');
      if (close) cm.closeSheet(close);
    }
  });

  window.cm = {
    // Page direction for the list slide: set by the pager before its request.
    dir(d) { document.documentElement.dataset.dir = d; },

    // Closing the details sheet with its button or backdrop: slide it down
    // first, then let htmx swap. On desktop the panel morphs instead.
    async closeSheet(link) {
      const sheet = document.querySelector('.cm-sheet');
      if (sheet && !desktop() && !reduced()) {
        sheet.classList.add('cm-sheet-out');
        document.querySelector('.cm-backdrop')?.classList.add('cm-backdrop-out');
        await animated(sheet, 420);
      }
      cm.dir('select');
      link.dispatchEvent(new CustomEvent('cm-close'));
    },
  };

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', watch);
  else watch();
  document.addEventListener('htmx:after:swap', watch);
  // The Week/Agenda morph names every row while data-dir is "switch"; drop it
  // once the morph has played so later swaps don't split rows off.
  document.addEventListener('htmx:after:swap', () => {
    const root = document.documentElement;
    if (root.dataset.dir !== 'switch') return;
    clearTimeout(root.cmSwitch);
    root.cmSwitch = setTimeout(() => { if (root.dataset.dir === 'switch') delete root.dataset.dir; }, 1500);
  });
})();
