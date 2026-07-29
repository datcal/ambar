// The 2D viewer from spec §8: zoom, pan, and a background toggle.
//
// A JS island, as §2 requires: no bundler, no framework, plain ES module served from
// /static. All configuration arrives in data- attributes on the container, so the CSP
// stays `default-src 'self'` with no inline script and no nonce plumbing.
//
// It loads preview.webp rather than the original file. §11 forbids serving library
// content inline — an .svg or .html served from the app origin is stored XSS — and the
// preview is our own encoder's output, so it has no such surface. It also means this
// viewer works identically for PSD, SVG and Aseprite sources.

const ZOOM_MIN = 0.05;
const ZOOM_MAX = 32;

// Wheel zoom feels right at roughly 10% per notch; browsers report wildly different
// deltaY magnitudes, so only the sign is used.
const WHEEL_STEP = 1.1;

function init(root) {
  const stage = root.querySelector('[data-role="stage"]');
  const img = root.querySelector('[data-role="image"]');
  const status = root.querySelector('[data-role="status"]');
  if (!stage || !img) return;

  const stillSrc = root.dataset.src;
  const animSrc = root.dataset.animSrc || '';
  // §8: `image-rendering: pixelated` above 1x when the asset is pixel art. This is the
  // payoff for storing is_pixel_art at derive time.
  const pixelArt = root.dataset.pixelArt === 'true';

  const state = {
    zoom: 1,
    fit: true,
    panX: 0,
    panY: 0,
    dragging: false,
    dragStartX: 0,
    dragStartY: 0,
    animating: false,
  };

  function naturalSize() {
    // naturalWidth is 0 until the image loads; the indexed dimensions are the
    // fallback, and they are what the zoom percentage should really be relative to
    // anyway — the preview may have been downscaled from the original.
    const w = img.naturalWidth || Number(root.dataset.width) || 0;
    const h = img.naturalHeight || Number(root.dataset.height) || 0;
    return { w, h };
  }

  function fitScale() {
    const { w, h } = naturalSize();
    if (!w || !h) return 1;
    const rect = stage.getBoundingClientRect();
    if (!rect.width || !rect.height) return 1;
    // Never scale up to fit: a 16x16 sprite shown at 40x is not "fit", it is a
    // decision the user should make with the zoom buttons.
    return Math.min(1, Math.min(rect.width / w, rect.height / h));
  }

  function render() {
    const scale = state.fit ? fitScale() : state.zoom;

    img.style.transform =
      `translate(${state.panX}px, ${state.panY}px) scale(${scale})`;
    // Smoothing off above 1:1 for pixel art, and always off below it would blur a
    // downscale that the browser does better smoothly.
    img.style.imageRendering = pixelArt && scale > 1 ? 'pixelated' : 'auto';

    if (status) {
      const { w, h } = naturalSize();
      const percent = Math.round(scale * 100);
      const parts = [];
      if (w && h) parts.push(`${w}×${h}`);
      parts.push(state.fit ? `fit (${percent}%)` : `${percent}%`);
      if (pixelArt) parts.push('pixel art');
      status.textContent = parts.join(' · ');
    }

    for (const button of root.querySelectorAll('[data-zoom]')) {
      const target = button.dataset.zoom;
      const active = state.fit ? target === 'fit' : Number(target) === state.zoom;
      button.classList.toggle('on', active);
    }
  }

  function setZoom(value, originX, originY) {
    const clamped = Math.min(ZOOM_MAX, Math.max(ZOOM_MIN, value));
    if (originX !== undefined) {
      // Keep the point under the cursor fixed, which is what makes wheel zoom feel
      // like zooming rather than jumping.
      const previous = state.fit ? fitScale() : state.zoom;
      const ratio = clamped / previous;
      state.panX = originX - ratio * (originX - state.panX);
      state.panY = originY - ratio * (originY - state.panY);
    }
    state.zoom = clamped;
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

    if (button.dataset.toggle === 'anim' && animSrc) {
      state.animating = !state.animating;
      img.src = state.animating ? animSrc : stillSrc;
      button.textContent = state.animating ? 'Pause' : 'Play';
      button.classList.toggle('on', state.animating);
    }
  });

  // --- wheel zoom ---

  stage.addEventListener(
    'wheel',
    (event) => {
      event.preventDefault();
      const rect = stage.getBoundingClientRect();
      const originX = event.clientX - rect.left;
      const originY = event.clientY - rect.top;
      const current = state.fit ? fitScale() : state.zoom;
      setZoom(event.deltaY < 0 ? current * WHEEL_STEP : current / WHEEL_STEP, originX, originY);
    },
    // Not passive: the whole point is to preventDefault so the page does not scroll.
    { passive: false },
  );

  // --- drag to pan ---

  stage.addEventListener('pointerdown', (event) => {
    state.dragging = true;
    state.dragStartX = event.clientX - state.panX;
    state.dragStartY = event.clientY - state.panY;
    stage.setPointerCapture(event.pointerId);
    stage.classList.add('dragging');
  });

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
        setZoom((state.fit ? fitScale() : state.zoom) * WHEEL_STEP);
        break;
      case '-':
        setZoom((state.fit ? fitScale() : state.zoom) / WHEEL_STEP);
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

  // Refit when the image finally loads and when the window changes shape.
  img.addEventListener('load', render);
  window.addEventListener('resize', () => {
    if (state.fit) render();
  });

  stage.dataset.bg = 'checker';
  const checkerButton = root.querySelector('[data-bg="checker"]');
  if (checkerButton) checkerButton.classList.add('on');

  render();
}

for (const root of document.querySelectorAll('.viewer')) {
  init(root);
}
