// §8 3D viewer: load the normalised preview.glb into three.js with orbit
// controls, grid and axis helpers, a wireframe toggle and a 1.8 m human-height
// scale reference. Uses the vendored global THREE (r128) — no CDN, so the §11
// CSP is satisfied. A no-op on pages without #model-viewer.

(function () {
  const root = document.getElementById("model-viewer");
  if (!root || typeof THREE === "undefined") return;

  const canvas = root.querySelector('[data-role="canvas"]');
  const stage = root.querySelector('[data-role="stage"]');
  const src = root.dataset.src;

  const renderer = new THREE.WebGLRenderer({ canvas: canvas, antialias: true });
  renderer.setPixelRatio(window.devicePixelRatio || 1);

  const scene = new THREE.Scene();
  scene.background = new THREE.Color(0x16181c);

  const camera = new THREE.PerspectiveCamera(45, 1, 0.01, 1000);
  const controls = new THREE.OrbitControls(camera, renderer.domElement);
  controls.enableDamping = true;

  scene.add(new THREE.HemisphereLight(0xffffff, 0x333333, 1.1));
  const key = new THREE.DirectionalLight(0xffffff, 0.8);
  key.position.set(2, 3, 2);
  scene.add(key);

  const grid = new THREE.GridHelper(10, 10, 0x3a414d, 0x2c313a);
  scene.add(grid);
  const axes = new THREE.AxesHelper(1);
  scene.add(axes);

  // §8 scale reference: a 1.8 m human-height marker at the origin, so an asset
  // authored at the wrong scale is obvious at a glance.
  const scaleRef = new THREE.Mesh(
    new THREE.BoxGeometry(0.4, 1.8, 0.25),
    new THREE.MeshStandardMaterial({ color: 0x6ea8fe, transparent: true, opacity: 0.25 })
  );
  scaleRef.position.y = 0.9;
  scaleRef.visible = false;
  scene.add(scaleRef);

  let model = null;
  let wireframe = false;

  function resize() {
    const w = stage.clientWidth;
    const h = stage.clientHeight;
    renderer.setSize(w, h, false);
    camera.aspect = w / h || 1;
    camera.updateProjectionMatrix();
  }

  function frame(obj) {
    const box = new THREE.Box3().setFromObject(obj);
    const size = box.getSize(new THREE.Vector3());
    const center = box.getCenter(new THREE.Vector3());
    const maxDim = Math.max(size.x, size.y, size.z) || 1;

    controls.target.copy(center);
    camera.position.set(center.x + maxDim * 1.4, center.y + maxDim * 0.9, center.z + maxDim * 1.8);
    camera.near = maxDim / 100;
    camera.far = maxDim * 100;
    camera.updateProjectionMatrix();
    controls.update();

    // Size the grid and axes to the model.
    grid.scale.setScalar(Math.max(1, maxDim));
    axes.scale.setScalar(maxDim * 0.6);
  }

  function setWireframe(on) {
    wireframe = on;
    if (!model) return;
    model.traverse(function (o) {
      if (o.isMesh && o.material) {
        (Array.isArray(o.material) ? o.material : [o.material]).forEach(function (m) {
          m.wireframe = on;
        });
      }
    });
  }

  root.querySelector('[data-toggle="wire"]').addEventListener("click", function (e) {
    setWireframe(!wireframe);
    e.currentTarget.classList.toggle("on", wireframe);
  });
  root.querySelector('[data-toggle="scale"]').addEventListener("click", function (e) {
    scaleRef.visible = !scaleRef.visible;
    e.currentTarget.classList.toggle("on", scaleRef.visible);
  });

  new THREE.GLTFLoader().load(
    src,
    function (gltf) {
      model = gltf.scene;
      scene.add(model);
      frame(model);
    },
    undefined,
    function (err) {
      const status = root.querySelector('[data-role="status"]');
      if (status) status.textContent = "Could not load the model.";
      console.error(err);
    }
  );

  function animate() {
    requestAnimationFrame(animate);
    controls.update();
    renderer.render(scene, camera);
  }

  window.addEventListener("resize", resize);
  resize();
  animate();
})();
