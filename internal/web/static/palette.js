// The palette panel from spec §8: click-to-copy, a copy-format preference, a
// frequency/perceptual sort toggle, and a greyscale check.
//
// A JS island like the other viewers: no bundler, no framework, all state from data-
// attributes so the CSP stays `default-src 'self'`. Swatch colours are set through the
// CSSOM here rather than an inline style attribute, which the CSP would otherwise block.
//
// The single most-used interaction is copy, and §8 is explicit that it must not silently
// fail on a plain-HTTP LAN, where navigator.clipboard is unavailable outside a secure
// context. So copy tries the async Clipboard API first and falls back to a
// hidden-textarea document.execCommand('copy').
//
// M16 split the panel in two. The strip is circles — the palette as a palette, one click
// to copy — and the numbers, the sort, the greyscale check and the exports moved behind
// a "Details" toggle. Both halves are driven from the same DOM, so a sort applies to the
// strip and the table together and they can never disagree.

const COPY_PREF_KEY = "ambar.palette.copyFormat";
const DETAILS_PREF_KEY = "ambar.palette.detailsOpen";

// formatColor renders one swatch in the chosen copy format. The GDScript form mirrors the
// server's palette.GDColor exactly (three decimals), so a value copied here is
// byte-identical to one exported to a .gd file.
function formatColor(el, fmt) {
  const hex = el.dataset.hex || el.dataset.copy;
  const r = Number(el.dataset.r);
  const g = Number(el.dataset.g);
  const b = Number(el.dataset.b);
  switch (fmt) {
    case "hexbare":
      return hex.replace("#", "");
    case "rgb":
      return `rgb(${r}, ${g}, ${b})`;
    case "rgbbare":
      return `${r}, ${g}, ${b}`;
    case "gd":
      return `Color(${norm(r)}, ${norm(g)}, ${norm(b)})`;
    default:
      return hex;
  }
}

function norm(v) {
  return (v / 255).toFixed(3);
}

// copyText copies to the clipboard, returning whether it succeeded. The secure Clipboard
// API is preferred; the execCommand path is the LAN fallback (§8).
async function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch (e) {
      // Fall through to the legacy path.
    }
  }
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.setAttribute("readonly", "");
  ta.className = "offscreen-copy";
  document.body.appendChild(ta);
  ta.select();
  let ok = false;
  try {
    ok = document.execCommand("copy");
  } catch (e) {
    ok = false;
  }
  document.body.removeChild(ta);
  return ok;
}

// rgbToHsl returns hue 0..360, saturation and lightness 0..1.
function rgbToHsl(r, g, b) {
  r /= 255;
  g /= 255;
  b /= 255;
  const max = Math.max(r, g, b);
  const min = Math.min(r, g, b);
  const l = (max + min) / 2;
  let h = 0;
  let s = 0;
  const d = max - min;
  if (d !== 0) {
    s = l > 0.5 ? d / (2 - max - min) : d / (max + min);
    switch (max) {
      case r:
        h = ((g - b) / d + (g < b ? 6 : 0)) * 60;
        break;
      case g:
        h = ((b - r) / d + 2) * 60;
        break;
      default:
        h = ((r - g) / d + 4) * 60;
        break;
    }
  }
  return { h, s, l };
}

function readPref(key, fallback) {
  try {
    const raw = window.localStorage.getItem(key);
    return raw === null ? fallback : raw;
  } catch (e) {
    return fallback;
  }
}

function writePref(key, value) {
  try {
    window.localStorage.setItem(key, value);
  } catch (e) {
    // Private mode. The choice still applies to this page view.
  }
}

function init(root) {
  const list = root.querySelector('[data-role="swatches"]');
  const rows = root.querySelector('[data-role="rows"]');
  const status = root.querySelector('[data-role="status"]');
  const details = root.querySelector('[data-role="details"]');
  if (!list) return;

  const items = Array.from(list.children); // the <li> wrappers, in frequency order
  const dots = items.map((li) => li.querySelector(".dot"));
  const rowItems = rows ? Array.from(rows.children) : [];

  // Paint every circle from its data-hex — here rather than via an inline style, so the
  // CSP needs no 'unsafe-inline'.
  for (const dot of dots) {
    const fill = dot.querySelector(".dot-fill");
    if (fill) fill.style.backgroundColor = dot.dataset.hex;
  }
  for (const fill of root.querySelectorAll(".dot-sm[data-hex]")) {
    fill.style.backgroundColor = fill.dataset.hex;
  }

  // --- the Details disclosure ---
  const detailsButton = root.querySelector('[data-toggle="details"]');
  if (detailsButton && details) {
    const apply = (open) => {
      details.hidden = !open;
      detailsButton.classList.toggle("on", open);
      detailsButton.setAttribute("aria-expanded", open ? "true" : "false");
    };
    apply(readPref(DETAILS_PREF_KEY, "false") === "true");
    detailsButton.addEventListener("click", () => {
      const open = details.hidden;
      apply(open);
      writePref(DETAILS_PREF_KEY, open ? "true" : "false");
    });
  }

  // --- copy format preference, remembered across visits ---
  const formatSelect = root.querySelector('[data-role="copy-format"]');
  if (formatSelect) {
    const saved = readPref(COPY_PREF_KEY, "");
    if (saved) formatSelect.value = saved;
    formatSelect.addEventListener("change", () => {
      writePref(COPY_PREF_KEY, formatSelect.value);
    });
  }
  const currentFormat = () => (formatSelect ? formatSelect.value : "hex");

  let statusTimer = 0;
  function say(message) {
    if (!status) return;
    status.textContent = message;
    window.clearTimeout(statusTimer);
    statusTimer = window.setTimeout(() => {
      status.textContent = "";
    }, 2500);
  }

  function flash(el) {
    el.classList.add("copied");
    window.setTimeout(() => el.classList.remove("copied"), 600);
  }

  // --- click to copy, shift-click to copy a range (§8) ---
  let anchor = null; // index into the current display order
  function displayOrder() {
    return Array.from(list.children).map((li) => li.querySelector(".dot"));
  }

  for (const dot of dots) {
    dot.addEventListener("click", async (event) => {
      const order = displayOrder();
      const index = order.indexOf(dot);

      let text;
      let label;
      if (event.shiftKey && anchor !== null) {
        const [lo, hi] = anchor < index ? [anchor, index] : [index, anchor];
        const range = order.slice(lo, hi + 1);
        text = range.map((d) => formatColor(d, currentFormat())).join("\n");
        label = `Copied ${range.length} colours`;
        for (const d of range) flash(d);
      } else {
        anchor = index;
        text = formatColor(dot, currentFormat());
        label = `Copied ${text}`;
        flash(dot);
      }

      const ok = await copyText(text);
      say(ok ? label : "Copy failed — select and copy manually");
    });
  }

  // The copy icon on a details row. It copies in the selected format like everything
  // else, so the table and the strip cannot disagree about what "copy" means.
  for (const icon of root.querySelectorAll("[data-copy]")) {
    icon.addEventListener("click", async (event) => {
      event.preventDefault();
      const text = formatColor(icon, currentFormat());
      flash(icon);
      const ok = await copyText(text);
      say(ok ? `Copied ${text}` : "Copy failed — select and copy manually");
    });
  }

  // --- sort: frequency (original order) vs perceptual (hue then lightness) ---
  const sortButtons = root.querySelectorAll("[data-sort]");

  // Both views are sorted by the same comparison, keyed off the strip's order, so the
  // table always lists the colours in the order the circles show them.
  function perceptualIndex() {
    const order = items.map((li, i) => i);
    order.sort((ia, ib) => {
      const a = items[ia].querySelector(".dot");
      const b = items[ib].querySelector(".dot");
      const ha = rgbToHsl(Number(a.dataset.r), Number(a.dataset.g), Number(a.dataset.b));
      const hb = rgbToHsl(Number(b.dataset.r), Number(b.dataset.g), Number(b.dataset.b));
      // Near-achromatic colours have no meaningful hue; grouping them together and
      // ordering by lightness keeps the greys as their own ramp at the front rather than
      // scattering them across the wheel.
      const greyA = ha.s < 0.12;
      const greyB = hb.s < 0.12;
      if (greyA !== greyB) return greyA ? -1 : 1;
      if (greyA && greyB) return ha.l - hb.l;
      // Cluster into ~24 hue buckets so a ramp stays contiguous, then order each cluster
      // by lightness (§8: "reveals the ramp structure").
      const bucketA = Math.round(ha.h / 15);
      const bucketB = Math.round(hb.h / 15);
      if (bucketA !== bucketB) return bucketA - bucketB;
      return ha.l - hb.l;
    });
    return order;
  }

  const frequencyOrder = items.map((li, i) => i);
  const perceptualOrder = perceptualIndex();

  function applyOrder(order) {
    for (const i of order) {
      list.appendChild(items[i]);
      if (rowItems[i]) rows.appendChild(rowItems[i]);
    }
    anchor = null; // display order changed, so the old anchor is meaningless
  }

  for (const button of sortButtons) {
    button.addEventListener("click", () => {
      for (const b of sortButtons) b.classList.toggle("on", b === button);
      applyOrder(button.dataset.sort === "perceptual" ? perceptualOrder : frequencyOrder);
    });
  }

  // --- greyscale check ---
  const greyToggle = root.querySelector('[data-toggle="greyscale"]');
  if (greyToggle) {
    greyToggle.addEventListener("click", () => {
      const on = root.classList.toggle("greyscale");
      greyToggle.classList.toggle("on", on);
    });
  }
}

for (const root of document.querySelectorAll(".palette")) {
  init(root);
}
