// §8 keyboard audition mode: step through the audio results with the arrow keys,
// each playing immediately, and tag the current one with a single keystroke.
// "Auditioning 400 impact sounds with a mouse is unbearable" — this is the island
// that fixes it. It stays dormant until the page actually has audio tiles.

(function () {
  const bar = document.getElementById("audition");
  if (!bar) return;

  function audioTiles() {
    return Array.from(document.querySelectorAll("#thumbgrid [data-audio]"));
  }
  if (audioTiles().length === 0) return; // no audio in this result set
  bar.hidden = false;

  const toggleBtn = bar.querySelector('[data-role="toggle"]');
  const nowEl = bar.querySelector('[data-role="now"]');
  const quicktag = bar.querySelector('[data-role="quicktag"]');
  const csrf = (document.querySelector('meta[name="csrf-token"]') || {}).content || "";
  const player = new Audio();
  player.loop = false;

  let enabled = false;
  let tiles = [];
  let index = -1;

  function setEnabled(on) {
    enabled = on;
    toggleBtn.classList.toggle("on", on);
    toggleBtn.textContent = on ? "■ Stop audition (a)" : "▶ Audition audio (a)";
    if (on) {
      tiles = audioTiles(); // re-read: load-more may have appended more
      if (tiles.length) select(0);
    } else {
      player.pause();
      clearSelection();
      nowEl.textContent = "";
    }
  }

  function clearSelection() {
    tiles.forEach((t) => t.classList.remove("auditioning"));
  }

  function select(i) {
    if (!tiles.length) return;
    index = (i + tiles.length) % tiles.length;
    clearSelection();
    const tile = tiles[index];
    tile.classList.add("auditioning");
    tile.scrollIntoView({ block: "nearest", behavior: "smooth" });
    nowEl.textContent = tile.dataset.title || "";
    player.src = tile.dataset.audio;
    player.play().catch(() => {});
  }

  function applyQuickTag() {
    const tag = quicktag.value.trim();
    if (!tag || index < 0) return;
    const id = tiles[index].dataset.assetId;
    const body = new URLSearchParams();
    body.set("tag", tag);
    fetch("/assets/" + id + "/tags", {
      method: "POST",
      headers: { "X-CSRF-Token": csrf, "Content-Type": "application/x-www-form-urlencoded" },
      body: body.toString(),
    })
      .then((r) => {
        const tile = tiles[index];
        tile.classList.add(r.ok ? "tagged" : "tag-failed");
        setTimeout(() => tile.classList.remove("tagged", "tag-failed"), 800);
      })
      .catch(() => {});
  }

  toggleBtn.addEventListener("click", () => setEnabled(!enabled));

  document.addEventListener("keydown", (e) => {
    const t = e.target;
    const typing = t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.isContentEditable);

    // Enter inside the quick-tag box applies it without leaving the box.
    if (typing) {
      if (t === quicktag && e.key === "Enter") {
        e.preventDefault();
        applyQuickTag();
      }
      return;
    }

    if (e.key === "a" || e.key === "A") {
      e.preventDefault();
      setEnabled(!enabled);
      return;
    }
    if (!enabled) return;

    switch (e.key) {
      case "ArrowDown":
      case "ArrowRight":
        e.preventDefault();
        select(index + 1);
        break;
      case "ArrowUp":
      case "ArrowLeft":
        e.preventDefault();
        select(index - 1);
        break;
      case " ":
        e.preventDefault();
        if (player.paused) player.play().catch(() => {});
        else player.pause();
        break;
      case "Escape":
        e.preventDefault();
        setEnabled(false);
        break;
      case "t":
      case "T":
        e.preventDefault();
        applyQuickTag();
        break;
    }
  });
})();
