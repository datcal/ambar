// §8 3D viewer: load a model into three.js with orbit controls, grid and axis
// helpers, a wireframe toggle and a 1.8 m human-height scale reference. Uses the
// vendored global THREE (r128) — no CDN, so the §11 CSP is satisfied. A no-op on
// pages without #model-viewer.
//
// M14: the format decides the loader. glTF/GLB load through GLTFLoader as before
// (a derived preview.glb when there is one), and .obj and .fbx now load *directly*
// from the library through /assets/{id}/file/, which is what removed the "this needs
// Blender to preview" dead end from every OBJ and FBX in the library. Companion
// files — an .obj's .mtl, an .mtl's textures, a .gltf's .bin — resolve relative to
// that URL without the viewer rewriting anything.
//
// M15 fix: a texture that cannot be fetched used to make the model *invisible* rather
// than untextured. three.js binds a 1x1 transparent-black placeholder for a texture
// whose image never arrived, and `colour x rgba(0,0,0,0)` is nothing at all — so an FBX
// naming a texture by an artist's Windows path rendered as an empty stage with no error
// anywhere. Every .fbx in this library does that. Hence two things below: the loaders
// share a LoadingManager so the viewer knows when fetching has actually finished, and a
// texture that never arrived is dropped so the geometry shows in its base colour. An
// untextured barrel answers "what is this"; an empty stage does not.

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

  // Shared by every loader below, so "has all fetching finished" is answerable. The
  // model file itself is a tracked item, and the textures a loader starts during parse
  // are tracked too, so onLoad fires after the whole set has either arrived or failed.
  const manager = new THREE.LoadingManager();
  let settled = false;
  const missing = [];

  // Texture slots worth checking. Any of them left holding a placeholder is enough to
  // turn a mesh invisible, so they are all cleared rather than just `map`.
  const TEXTURE_SLOTS = [
    "map", "specularMap", "emissiveMap", "normalMap", "bumpMap",
    "aoMap", "alphaMap", "metalnessMap", "roughnessMap", "envMap",
  ];

  function textureArrived(texture) {
    const image = texture && texture.image;
    if (!image) return false;
    if (typeof image.complete === "boolean" && !image.complete) return false;
    // An <img>, an ImageBitmap, a canvas, or raw data — any of these is usable.
    return (image.width || 0) > 0 || !!image.data;
  }

  // Drop textures that will never arrive. Only ever called once fetching has settled,
  // so a texture still in flight is never thrown away.
  function dropMissingTextures(object) {
    let dropped = 0;
    object.traverse(function (o) {
      if (!o.material) return;
      (Array.isArray(o.material) ? o.material : [o.material]).forEach(function (m) {
        TEXTURE_SLOTS.forEach(function (slot) {
          if (m[slot] && !textureArrived(m[slot])) {
            m[slot] = null;
            m.needsUpdate = true;
            dropped += 1;
          }
        });
      });
    });
    return dropped;
  }

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

  const status = root.querySelector('[data-role="status"]');

  function fail(what, err) {
    if (status) status.textContent = what;
    console.error(err);
  }

  function show(object) {
    model = object;
    scene.add(model);
    frame(model);
    if (status) status.textContent = "";
    settle();
  }

  // Runs when both the model and its fetches are done, in whichever order that
  // happens. Says so when a texture had to be dropped: a white model is otherwise
  // baffling, and "the texture is missing" is something the user can act on.
  function settle() {
    if (!settled || !model) return;
    const dropped = dropMissingTextures(model);
    if (dropped > 0 && status) {
      const what = missing.length === 1 ? missing[0].split("/").pop() : dropped + " textures";
      status.textContent = "Showing untextured geometry: " + what + " could not be found beside this model.";
    }
  }

  manager.onError = function (url) { missing.push(url); };
  manager.onLoad = function () { settled = true; settle(); };
  // Backstop: a request that never completes would otherwise leave an invisible model
  // on screen for ever.
  window.setTimeout(function () { settled = true; settle(); }, 10000);

  // The format, as the page worked it out. Defaults to glTF so an older page that
  // only knows about preview.glb keeps working.
  const format = (root.dataset.format || "gltf").toLowerCase();

  if (status) status.textContent = "Loading…";

  switch (format) {
    case "obj": {
      // An .obj names its material library; MTLLoader resolves the textures the .mtl
      // names. Both are fetched from the same directory as the model, and a missing
      // .mtl is not fatal — untextured geometry still answers "what is this".
      const mtlName = root.dataset.mtl;
      const base = src.slice(0, src.lastIndexOf("/") + 1);
      const loadObj = function (materials) {
        const loader = new THREE.OBJLoader(manager);
        if (materials) loader.setMaterials(materials);
        loader.load(src, show, undefined, function (err) {
          fail("Could not read this .obj file.", err);
        });
      };
      if (mtlName) {
        new THREE.MTLLoader(manager)
          .setPath(base)
          .load(mtlName, function (materials) {
            materials.preload();
            loadObj(materials);
          }, undefined, function () {
            // No usable .mtl: carry on without materials rather than showing nothing.
            loadObj(null);
          });
      } else {
        loadObj(null);
      }
      break;
    }

    case "fbx":
      if (typeof THREE.FBXLoader === "undefined") {
        fail("The FBX loader did not load.", null);
        break;
      }
      new THREE.FBXLoader(manager).load(src, show, undefined, function (err) {
        fail("Could not read this .fbx file.", err);
      });
      break;

    default:
      new THREE.GLTFLoader(manager).load(
        src,
        function (gltf) {
          show(gltf.scene);
        },
        undefined,
        function (err) {
          fail("Could not load the model.", err);
        }
      );
  }

  function animate() {
    requestAnimationFrame(animate);
    controls.update();
    renderer.render(scene, camera);
  }

  window.addEventListener("resize", resize);
  resize();
  animate();
})();
