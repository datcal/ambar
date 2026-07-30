// §8 audio viewer: draw a waveform from the stored peaks, play the original, seek
// by clicking, toggle a loop. An isolated island — it does nothing on pages with
// no #audio-viewer, so loading it everywhere costs one cached request.

(function () {
  const root = document.getElementById("audio-viewer");
  if (!root) return;

  const canvas = root.querySelector('[data-role="waveform"]');
  const player = root.querySelector('[data-role="player"]');
  const playBtn = root.querySelector('[data-role="play"]');
  const loopBtn = root.querySelector('[data-role="loop"]');
  const timeEl = root.querySelector('[data-role="time"]');
  const ctx = canvas.getContext("2d");

  let peaks = { count: 0, min: [], max: [] };

  player.loop = root.dataset.loopable === "true";
  if (player.loop) loopBtn.classList.add("on");

  function themeColors() {
    const dark = !document.documentElement.dataset.theme
      ? matchMedia("(prefers-color-scheme: dark)").matches
      : document.documentElement.dataset.theme === "dark";
    return dark
      ? { played: "#6ea8fe", bg: "#3a414d" }
      : { played: "#2563eb", bg: "#c2c8d2" };
  }

  function resize() {
    // Match the backing store to the CSS pixel size for a crisp line.
    const ratio = window.devicePixelRatio || 1;
    canvas.width = Math.max(1, Math.floor(canvas.clientWidth * ratio));
    canvas.height = Math.max(1, Math.floor(canvas.clientHeight * ratio));
    draw();
  }

  function draw() {
    const w = canvas.width;
    const h = canvas.height;
    const mid = h / 2;
    const n = peaks.count;
    const colors = themeColors();
    ctx.clearRect(0, 0, w, h);
    if (n === 0) return;

    const progress = player.duration ? player.currentTime / player.duration : 0;
    const progressX = progress * w;
    for (let x = 0; x < w; x++) {
      const i = Math.min(n - 1, Math.floor((x / w) * n));
      const top = mid - peaks.max[i] * mid;
      const bot = mid - peaks.min[i] * mid;
      ctx.strokeStyle = x <= progressX ? colors.played : colors.bg;
      ctx.beginPath();
      ctx.moveTo(x + 0.5, top);
      ctx.lineTo(x + 0.5, Math.max(bot, top + 1));
      ctx.stroke();
    }
  }

  function fmt(sec) {
    if (!isFinite(sec)) return "0:00";
    const s = Math.floor(sec);
    return Math.floor(s / 60) + ":" + String(s % 60).padStart(2, "0");
  }

  function togglePlay() {
    if (player.paused) player.play();
    else player.pause();
  }

  playBtn.addEventListener("click", togglePlay);
  loopBtn.addEventListener("click", function () {
    player.loop = !player.loop;
    loopBtn.classList.toggle("on", player.loop);
  });

  player.addEventListener("play", function () {
    playBtn.textContent = "❚❚ Pause";
  });
  player.addEventListener("pause", function () {
    playBtn.textContent = "▶ Play";
  });
  player.addEventListener("timeupdate", function () {
    timeEl.textContent = fmt(player.currentTime);
    draw();
  });
  player.addEventListener("ended", draw);

  canvas.addEventListener("click", function (e) {
    if (!player.duration) return;
    const rect = canvas.getBoundingClientRect();
    player.currentTime = ((e.clientX - rect.left) / rect.width) * player.duration;
  });

  // Space plays/pauses, unless the user is typing in a field.
  document.addEventListener("keydown", function (e) {
    if (e.code !== "Space") return;
    const t = e.target;
    if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.isContentEditable)) return;
    e.preventDefault();
    togglePlay();
  });

  window.addEventListener("resize", resize);

  fetch(root.dataset.peaks, { headers: { Accept: "application/json" } })
    .then(function (r) { return r.ok ? r.json() : null; })
    .then(function (data) {
      if (data && data.max) peaks = data;
      resize();
    })
    .catch(function () { resize(); });
})();
