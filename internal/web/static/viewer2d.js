// The 2D viewer from spec §8: zoom, pan, background toggle, and pixel-exact rendering.
//
// A JS island, as §2 requires: no bundler, no framework, plain ES module served from
// /static. All configuration arrives in data- attributes on the container, so the CSP
// stays `default-src 'self'` with no inline script and no nonce plumbing.
//
// It loads preview.webp rather than the original file. §11 forbids serving library
// content inline — an .svg or .html served from the app origin is stored XSS — and the
// preview is our own encoder's output, so it has no such surface. It also means this
// viewer works identically for PSD, SVG and Aseprite sources.
//
// Three rules here are deliberate reversals of the first version (M16):
//
//   - The wheel scrolls the page. Zooming on plain wheel meant the palette and the tags
//     below the viewer were unreachable, because the stage covers most of the column and
//     preventDefault ate every scroll. Zoom is Ctrl/⌘+wheel, plus an opt-in toolbar
//     switch for people who want the old behaviour.
//   - Rendering defaults to pixels, not to whatever the detector guessed. This is a
//     pixel-art library; smoothing is the exception, and a heuristic must not be the only
//     way to say so. The detector's answer becomes the initial hint, the switch wins.
//   - Centring is computed here rather than with a CSS translate. Mixing
//     `translate: -50% -50%` with a scaled `transform` offsets the image by
//     (scale - 1) × size / 2, which is why every asset sat right and low of centre.

const ZOOM_MIN = 0.05;
const ZOOM_MAX = 32;

// Wheel zoom feels right at roughly 10% per notch; browsers report wildly different
// deltaY magnitudes, so only the sign is used.
const WHEEL_STEP = 1.1;

// How far "fit" is allowed to scale a small image up. A 4x4 icon blown across the whole
// stage is a wall of colour rather than a preview, so the growth stops here.
const MAX_FIT_UPSCALE = 16;

const PREF_PIXELS = 'ambar.viewer.pixels';
const PREF_WHEEL = 'ambar.viewer.wheelZoom';

// Preferences are per-browser, and a locked-down browser must not break the viewer.
function readPref(key, fallback) {
  try {
    const raw = window.localStorage.getItem(key);
    if (raw === null) return fallback;
    return raw === 'true';
  } catch (e) {
    return fallback;
  }
}

function writePref(key, value) {
  try {
    window.localStorage.setItem(key, value ? 'true' : 'false');
  } catch (e) {
    // Not fatal: the choice still applies to this page view.
  }
}

function init(root) {
  const stage = root.querySelector('[data-role="stage"]');
  const img = root.querySelector('[data-role="image"]');
  const status = root.querySelector('[data-role="status"]');
  if (!stage || !img) return;

  const stillSrc = root.dataset.src;
  const animSrc = root.dataset.animSrc || '';
  // The detector's answer (§6's is_pixel_art) is only the default for a first visit:
  // once someone has used the switch, their choice is what applies everywhere.
  const detectedPixelArt = root.dataset.pixelArt === 'true';

  const state = {
    zoom: 1,
    fit: true,
    panX: 0,
    panY: 0,
    dragging: false,
    dragStartX: 0,
    dragStartY: 0,
    animating: false,
    pixels: readPref(PREF_PIXELS, true),
    wheelZoom: readPref(PREF_WHEEL, false),
  };

  function naturalSize() {
    // naturalWidth is 0 until the image loads; the indexed dimensions are the
    // fallback, and they are what the zoom percentage should really be relative to
    // anyway — the preview may have been downscaled from the original.
    const w = img.naturalWidth || Number(root.dataset.width) || 0;
    const h = img.naturalHeight || Number(root.dataset.height) || 0;
    return { w, h };
  }

  // --- scale ---

  // snapFit lands on a whole factor in pixels mode, rounding *down* so the image still
  // fits: 3x keeps every authored pixel a clean square, while 3.7x resamples every edge.
  // Below 1:1 the same rule applies to the divisor — 1/2, 1/3, 1/4 — which is what
  // Aseprite does and the reason a zoomed-out tileset stays legible.
  function snapFit(raw) {
    if (!state.pixels) return raw;
    if (raw >= 1) return Math.max(1, Math.floor(raw));
    return 1 / Math.max(2, Math.ceil(1 / raw));
  }

  // snapZoom is the same idea for a deliberate zoom, where nearest is friendlier than
  // always-down: a wheel notch should move rather than stick.
  function snapZoom(raw) {
    if (!state.pixels) return raw;
    if (raw >= 1) return Math.max(1, Math.round(raw));
    return 1 / Math.max(2, Math.round(1 / raw));
  }

  function fitScale() {
    const { w, h } = naturalSize();
    if (!w || !h) return 1;
    const rect = stage.getBoundingClientRect();
    if (!rect.width || !rect.height) return 1;

    // Fit scales up as well as down: a 20x21 tile opened at 20x21 in a 46rem stage is
    // a speck, which is exactly what the grid used to get wrong too.
    const raw = Math.min(rect.width / w, rect.height / h);
    return snapFit(Math.min(raw, MAX_FIT_UPSCALE));
  }

  function currentScale() {
    return state.fit ? fitScale() : state.zoom;
  }

  function clampZoom(value) {
    return Math.min(ZOOM_MAX, Math.max(ZOOM_MIN, value));
  }

  // --- painting ---

  function render() {
    const scale = currentScale();
    const { w, h } = naturalSize();
    const rect = stage.getBoundingClientRect();

    // The whole centring rule, in one place: the scaled image's top-left goes to
    // (stage - scaled size) / 2, so its centre is the stage's centre at every scale.
    // Rounded to whole device pixels, because a half-pixel offset re-samples pixel art
    // even when the scale itself is exact.
    const tx = Math.round((rect.width - w * scale) / 2 + state.panX);
    const ty = Math.round((rect.height - h * scale) / 2 + state.panY);

    img.style.transform = `translate(${tx}px, ${ty}px) scale(${scale})`;
    // Pixels mode never smooths, at any scale. The old `scale > 1` condition meant a
    // 2048px atlas shown at fit was blurred on the way down, which is the case where
    // sharpness matters most.
    img.style.imageRendering = state.pixels ? 'pixelated' : 'auto';

    if (status) {
      const parts = [];
      if (w && h) parts.push(`${w}×${h}`);
      const percent = Math.round(scale * 100);
      parts.push(state.fit ? `fit (${percent}%)` : `${percent}%`);
      parts.push(state.pixels ? 'pixels' : 'smooth');
      status.textContent = parts.join(' · ');
    }

    for (const button of root.querySelectorAll('[data-zoom]')) {
      const target = button.dataset.zoom;
      const active = state.fit ? target === 'fit' : Number(target) === state.zoom;
      button.classList.toggle('on', active);
    }
  }

  function setZoom(value, originX, originY) {
    const next = clampZoom(snapZoom(value));
    const { w, h } = naturalSize();
    const rect = stage.getBoundingClientRect();

    if (originX !== undefined) {
      // Keep the image point under the cursor fixed, which is what makes wheel zoom
      // feel like zooming rather than jumping. Solved against the same centring rule
      // render() uses, so the two can never drift apart.
      const previous = currentScale();
      const txBefore = (rect.width - w * previous) / 2 + state.panX;
      const tyBefore = (rect.height - h * previous) / 2 + state.panY;
      const pointX = (originX - txBefore) / previous;
      const pointY = (originY - tyBefore) / previous;
      state.panX = originX - next * pointX - (rect.width - w * next) / 2;
      state.panY = originY - next * pointY - (rect.height - h * next) / 2;
    }

    state.zoom = next;
    state.fit = false;
    render();
  }

  function reset() {
    state.fit = true;
    state.zoom = 1;
    state.panX = 0;
    state.panY = 0;
    render();
  }

  // --- toolbar ---

  function syncSwitches() {
    const pixelsButton = root.querySelector('[data-toggle="pixels"]');
    if (pixelsButton) {
      pixelsButton.classList.toggle('on', state.pixels);
      pixelsButton.textContent = state.pixels ? 'Pixels' : 'Smooth';
      pixelsButton.setAttribute('aria-pressed', state.pixels ? 'true' : 'false');
    }
    const wheelButton = root.querySelector('[data-toggle="wheel"]');
    if (wheelButton) {
      wheelButton.classList.toggle('on', state.wheelZoom);
      wheelButton.setAttribute('aria-pressed', state.wheelZoom ? 'true' : 'false');
    }
  }

  root.addEventListener('click', (event) => {
    const button = event.target.closest('button');
    if (!button || !root.contains(button)) return;

    if (button.dataset.zoom !== undefined) {
      if (button.dataset.zoom === 'fit') {
        reset();
      } else {
        state.panX = 0;
        state.panY = 0;
        setZoom(Number(button.dataset.zoom));
      }
      return;
    }

    if (button.dataset.bg !== undefined) {
      // §8's background toggle: checkerboard, black, white, mid-grey.
      stage.dataset.bg = button.dataset.bg;
      for (const other of root.querySelectorAll('[data-bg]')) {
        other.classList.toggle('on', other === button);
      }
      return;
    }

    if (button.dataset.toggle === 'pixels') {
      state.pixels = !state.pixels;
      writePref(PREF_PIXELS, state.pixels);
      // Snapping changed, so a non-fit zoom may now be off-grid; re-snap it.
      if (!state.fit) state.zoom = clampZoom(snapZoom(state.zoom));
      syncSwitches();
      render();
      return;
    }

    if (button.dataset.toggle === 'wheel') {
      state.wheelZoom = !state.wheelZoom;
      writePref(PREF_WHEEL, state.wheelZoom);
      syncSwitches();
      return;
    }

    if (button.dataset.toggle === 'anim' && animSrc) {
      state.animating = !state.animating;
      img.src = state.animating ? animSrc : stillSrc;
      button.textContent = state.animating ? 'Pause' : 'Play';
      button.classList.toggle('on', state.animating);
    }
  });

  // --- wheel ---

  stage.addEventListener(
    'wheel',
    (event) => {
      // Plain wheel belongs to the page. Without this the palette and the tags under
      // the viewer could only be reached with the scrollbar.
      if (!state.wheelZoom && !event.ctrlKey && !event.metaKey) return;

      event.preventDefault();
      const rect = stage.getBoundingClientRect();
      const originX = event.clientX - rect.left;
      const originY = event.clientY - rect.top;
      const current = currentScale();
      setZoom(event.deltaY < 0 ? current * WHEEL_STEP : current / WHEEL_STEP, originX, originY);
    },
    // Not passive: preventDefault is conditional, but it has to be allowed.
    { passive: false },
  );

  // --- drag to pan ---

  stage.addEventListener('pointerdown', (event) => {
    // preventDefault is what makes dragging *pan* instead of picking the image up.
    // Without it the browser starts its own native image drag the moment the pointer
    // moves over the <img>, so panning only worked from the empty space around it —
    // which is exactly the wrong way round.
    event.preventDefault();
    state.dragging = true;
    state.dragStartX = event.clientX - state.panX;
    state.dragStartY = event.clientY - state.panY;
    stage.setPointerCapture(event.pointerId);
    stage.classList.add('dragging');
    // Focus the stage so the arrow keys and +/- work straight after a drag.
    if (typeof stage.focus === 'function') stage.focus({ preventScroll: true });
  });

  // Belt and braces: some browsers begin a drag from mousedown rather than from the
  // pointer events above, and an <img> is draggable by default.
  img.setAttribute('draggable', 'false');
  stage.addEventListener('dragstart', (event) => event.preventDefault());

  stage.addEventListener('pointermove', (event) => {
    if (!state.dragging) return;
    state.panX = event.clientX - state.dragStartX;
    state.panY = event.clientY - state.dragStartY;
    render();
  });

  const endDrag = (event) => {
    if (!state.dragging) return;
    state.dragging = false;
    stage.classList.remove('dragging');
    if (event.pointerId !== undefined && stage.hasPointerCapture(event.pointerId)) {
      stage.releasePointerCapture(event.pointerId);
    }
  };
  stage.addEventListener('pointerup', endDrag);
  stage.addEventListener('pointercancel', endDrag);

  // --- keyboard ---
  //
  // §8 wants full keyboard navigation in the grid; here it is the minimum that makes
  // the viewer usable without a mouse.
  stage.addEventListener('keydown', (event) => {
    const step = event.shiftKey ? 50 : 10;
    switch (event.key) {
      case '0':
        reset();
        break;
      case '+':
      case '=':
        setZoom(currentScale() * WHEEL_STEP);
        break;
      case '-':
        setZoom(currentScale() / WHEEL_STEP);
        break;
      case 'p':
        state.pixels = !state.pixels;
        writePref(PREF_PIXELS, state.pixels);
        syncSwitches();
        break;
      case 'ArrowLeft':
        state.panX += step;
        break;
      case 'ArrowRight':
        state.panX -= step;
        break;
      case 'ArrowUp':
        state.panY += step;
        break;
      case 'ArrowDown':
        state.panY -= step;
        break;
      default:
        return;
    }
    event.preventDefault();
    render();
  });

  // Refit when the image finally loads, and re-centre whenever the stage changes shape —
  // centring is measured from the stage, so this matters even when not fitting.
  img.addEventListener('load', render);
  window.addEventListener('resize', render);

  stage.dataset.bg = 'checker';
  const checkerButton = root.querySelector('[data-bg="checker"]');
  if (checkerButton) checkerButton.classList.add('on');

  // The detector's answer is exposed for the status line and for anyone debugging a
  // misclassification; it no longer decides how the image is drawn.
  if (detectedPixelArt) root.dataset.detected = 'pixel-art';

  syncSwitches();
  render();
}

for (const root of document.querySelectorAll('.viewer')) {
  init(root);
}
