// The palette panel from spec §8: click-to-copy, a copy-format preference, a
// frequency/perceptual sort toggle, and a greyscale check.
//
// A JS island like the other viewers: no bundler, no framework, all state from
// data- attributes so the CSP stays `default-src 'self'`. Swatch chip colours are
// set through the CSSOM here rather than an inline style attribute, which the CSP
// would otherwise block.
//
// The single most-used interaction is copy, and §8 is explicit that it must not
// silently fail on a plain-HTTP LAN, where navigator.clipboard is unavailable
// outside a secure context. So copy tries the async Clipboard API first and falls
// back to a hidden-textarea document.execCommand('copy').

const COPY_PREF_KEY = "ambar.palette.copyFormat";

// formatColor renders one swatch button in the chosen copy format. The GDScript
// form mirrors the server's palette.GDColor exactly (three decimals), so a value
// copied here is byte-identical to one exported to a .gd file.
function formatColor(btn, fmt) {
  const hex = btn.dataset.hex;
  const r = Number(btn.dataset.r);
  const g = Number(btn.dataset.g);
  const b = Number(btn.dataset.b);
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

// copyText copies to the clipboard, returning whether it succeeded. The secure
// Clipboard API is preferred; the execCommand path is the LAN fallback (§8).
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
  ta.style.position = "absolute";
  ta.style.left = "-9999px";
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

function init(root) {
  const list = root.querySelector('[data-role="swatches"]');
  const status = root.querySelector('[data-role="status"]');
  if (!list) return;

  const items = Array.from(list.children); // the <li> wrappers, in frequency order
  const buttons = items.map((li) => li.querySelector(".swatch"));

  // Paint each chip from its data-hex. Done here rather than via an inline style
  // so the CSP needs no 'unsafe-inline'.
  for (const btn of buttons) {
    const chip = btn.querySelector(".swatch-chip");
    if (chip) chip.style.backgroundColor = btn.dataset.hex;
  }

  // --- copy format preference, remembered across visits ---
  const formatSelect = root.querySelector('[data-role="copy-format"]');
  if (formatSelect) {
    const saved = localStorage.getItem(COPY_PREF_KEY);
    if (saved) formatSelect.value = saved;
    formatSelect.addEventListener("change", () => {
      localStorage.setItem(COPY_PREF_KEY, formatSelect.value);
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

  function flash(btn) {
    btn.classList.add("copied");
    window.setTimeout(() => btn.classList.remove("copied"), 600);
  }

  // --- click to copy, shift-click to copy a range (§8) ---
  let anchor = null; // index into the current display order
  function displayOrder() {
    return Array.from(list.children).map((li) => li.querySelector(".swatch"));
  }

  for (const btn of buttons) {
    btn.addEventListener("click", async (event) => {
      const order = displayOrder();
      const index = order.indexOf(btn);

      let text;
      let label;
      if (event.shiftKey && anchor !== null) {
        const [lo, hi] = anchor < index ? [anchor, index] : [index, anchor];
        const range = order.slice(lo, hi + 1);
        text = range.map((b) => formatColor(b, currentFormat())).join("\n");
        label = `Copied ${range.length} colours`;
        for (const b of range) flash(b);
      } else {
        anchor = index;
        text = formatColor(btn, currentFormat());
        label = `Copied ${text}`;
        flash(btn);
      }

      const ok = await copyText(text);
      say(ok ? label : "Copy failed — select and copy manually");
    });
  }

  // The explicit copy icon (M15). The chip itself still copies — that is the panel's
  // day-to-day action — but the icon makes it discoverable next to the search icon,
  // and it copies in the currently selected format like everything else here.
  for (const icon of root.querySelectorAll("[data-copy]")) {
    icon.addEventListener("click", async (event) => {
      event.preventDefault();
      const li = icon.closest("li");
      const swatch = li ? li.querySelector(".swatch") : null;
      const text = swatch ? formatColor(swatch, currentFormat()) : icon.dataset.copy;
      if (swatch) flash(swatch);
      const ok = await copyText(text);
      say(ok ? `Copied ${text}` : "Copy failed — select and copy manually");
    });
  }

  // --- sort: frequency (original order) vs perceptual (hue then lightness) ---
  const sortButtons = root.querySelectorAll("[data-sort]");
  const frequencyOrder = items.slice();

  const perceptualOrder = items.slice().sort((liA, liB) => {
    const a = liA.querySelector(".swatch");
    const b = liB.querySelector(".swatch");
    const ha = rgbToHsl(Number(a.dataset.r), Number(a.dataset.g), Number(a.dataset.b));
    const hb = rgbToHsl(Number(b.dataset.r), Number(b.dataset.g), Number(b.dataset.b));
    // Near-achromatic colours have no meaningful hue; grouping them together and
    // ordering by lightness keeps the greys as their own ramp at the front rather
    // than scattering them across the wheel.
    const greyA = ha.s < 0.12;
    const greyB = hb.s < 0.12;
    if (greyA !== greyB) return greyA ? -1 : 1;
    if (greyA && greyB) return ha.l - hb.l;
    // Cluster into ~24 hue buckets so a ramp stays contiguous, then order each
    // cluster by lightness (§8: "reveals the ramp structure").
    const bucketA = Math.round(ha.h / 15);
    const bucketB = Math.round(hb.h / 15);
    if (bucketA !== bucketB) return bucketA - bucketB;
    return ha.l - hb.l;
  });

  function applyOrder(order) {
    for (const li of order) list.appendChild(li);
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
