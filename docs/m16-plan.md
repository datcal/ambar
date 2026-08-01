# M16 — arayüz revizyonu, iş planı

Kaynak: 2026-07-31 gözden geçirmesi ve `docs/archive/2026-07-ui-review-questions.md`'deki cevaplar.
Sapmalar `docs/decisions.md`'ye adım adım işleniyor.

Her adım tek başına çalışır durumda bırakılır ve `make check` yeşil geçmeden kapanmaz.
Commit'i Burak atar.

Durum: `[ ]` bekliyor · `[~]` sürüyor · `[x]` bitti

---

## Sıra ve gerekçe

Görsel temel en başta, çünkü "genel olarak çok eski hissettiriyor" şemsiye şikayeti ve
sonraki her adım o dilin üstüne markup ekliyor. Token katmanı önce gelir ki sayfa bazlı
tasarım işi tek tek değil, hazır bir sistemin üstüne otursun.

---

## [x] Adım 0 — Viewer ve pixel art (bitti)

2D viewer'ın ortalama bugu, tekerlek davranışı, Pixels/Smooth anahtarı, tespit kuralının
gerçek kütüphane ölçümüyle kalibrasyonu, grid tile'larında koşulsuz `pixelated`.
Ayrıntı: `docs/decisions.md` → "M16 — the 2D viewer".

---

## [x] Adım 1 — Tasarım temeli (bitti)

Yapıldı: renk rolleri (`--link`/`--brand`/`--active`/`--danger`/`--ok`/`--warn`), tek tip
ölçeği, tek boşluk ölçeği, dört buton rolü (varsayılan ikincil, `.primary`, `.linkish`,
`.danger`), segmentli araç çubuğu grupları, tek `.on` durumu, panelde gerçek seçili
durumu, kontrast AA'ya çekildi, ölü (`.searchbar`, `.kinds`) ve çift tanımlı
(`.swatch-*`, `.button`, `h2`) CSS temizlendi, statik dosyalara `?v=` damgası +
dev/dirty build'de `no-cache`.

Ayrıntı: `docs/decisions.md` → "M16 — the visual language".

Doğrulama: headless Firefox ile gerçek sayfalar render edildi (kütüphane, asset, trash,
login). Not: `--screenshot` görüntü çözülmesini yarışıyor, aynı profille iki kez çekmek
gerekiyor — script `uishot.sh` olarak saklandı.

<details>
<summary>Adım 1'in orijinal kapsamı</summary>

## Adım 1 — Tasarım temeli

Token katmanı ve ortak bileşen dili. Markup'a mümkün olduğunca dokunmadan her sayfayı
tazeler.

- Renk sistemi: tek `--accent` yerine ayrı roller — link, aktif durum, vurgu, tehlike,
  başarı, uyarı. Yüzey katmanları (`--bg`, `--surface`, `--surface-raised`).
- Tipografi ölçeği: gövde 15px→14.5/16, başlıklar için gerçek hiyerarşi, `--muted`
  kontrastı WCAG AA'ya çıkar. Her şeyin 0.8–0.9rem gri olduğu durum biter.
- Yoğunluk: satır yükseklikleri, panel/kart padding'leri, `gap`'ler tek ölçekten.
- Buton dili: birincil / ikincil / sessiz / tehlikeli — dört sınıf. Şu anki beş farklı
  ad-hoc stil (`.button`, `.button-quiet`, `.linkish`, toolbar, palette-controls) buna
  indirgenir.
- Yıkıcı eylemler: kırmızı, ikonlu, ve `trash` purge için yazarak onay.
- Odak halkası ve `.on` aktif durumu tek yerden.
- Temizlik: `app.css`'te iki kez tanımlı `.swatch-actions`/`.swatch-icon` (843-867 ve
  1207-1230), ölü `.searchbar`/`.kinds`, kullanılmayan sınıflar.
- Statik dosyalara sürüm damgası (`server.go:318` şu an 1 saat cache, damga yok → deploy
  sonrası hard refresh gerekiyor).

Dosyalar: `app.css`, `server.go` (statik handler), gerekirse `base.html`.
Kabul: her sayfa görsel olarak tazelenmiş, hiçbir işlev bozulmamış, `make check` yeşil.

</details>

## [x] Adım 2 — Kabuk: menü ve navigasyon (bitti)

Yapıldı: sidebar `nav.html`'e taşındı ve asset sayfasında da aynısı görünüyor; asset
sayfasının kendi cümlecik-linkleri rail'de chip oldu; bakım sayfaları tek grupta,
sayaçlarıyla; topbar dört hedefe indi, aktif sayfa vurgusu var; `/status` sayı ızgarası
oldu; audition (girişsiz kaldığı için) tamamen kaldırıldı; "open in" üç paragrafı gitti;
boş durumlar CLI komutu yerine butona işaret ediyor; sol panelde "Scanned 23 min ago"
satırı var.

Yanında gelen: `sidebar.go` — sidebar agregasyonları için 60 sn TTL'li önbellek +
yazmada `invalidate()`. Adım 8'in yarısı burada bitti, çünkü paylaşılan sidebar olmadan
asset sayfası bu sorguları bedavaya çalıştırıyor olurdu.

Ayrıntı: `docs/decisions.md` → "M16 — the shell".

<details>
<summary>Adım 2'nin orijinal kapsamı</summary>

## Adım 2 — Kabuk: menü ve navigasyon

- Sol panel asset sayfasında da aynı kalır; "← back to the library" cümlecikleri gider.
- Bakım sayfaları tek grupta (`duplicates`, `junk`, `trash`, `jobs`); topbar'daki üçlü
  tekrar biter.
- Aktif sayfa vurgusu, breadcrumb.
- `/status` sadeleşir (kalıyor, ama dashboard gibi görünür).
- Yazı diyeti birinci dalga: kalıcı açıklama paragrafları tooltip'e ya da `/settings`'e.

Dosyalar: `base.html`, `assets.html`, `asset.html`, `index.html`, `app.css`.

</details>

## [x] Adım 3 — Asset sayfası (bitti)

Biten:
- Palet dairelere indi, tıkla kopyala, "Details" arkasında yüzde/piksel/sıralama/greyscale/export.
- Viewer kalan yüksekliği alıyor; palet ve etiketler artık kaydırmasız görünüyor.
- Details paneli sıkıştı, denetim alanları "File details" arkasında.
- PSD saydamlığı düzeldi: katmanlar birleştiriliyor, opak zemin atlanıyor, composite yedek
  olarak kalıyor. `flattenPSD` birim testleri + `0015_psd_alpha_repair.sql`.

Sonra tamamlanan:
- Önceki/sonraki asset: rail'de düğmeler + `j`/`k`/ok tuşları, filtreleri taşıyor,
  uçlarda duruyor. `index.Neighbours` + iki sunucu testi.
- Asset sayfasında lisans + kaynak linki (kısmi güncelleme, iki testle korunuyor).
  Adım 9'daki `/provenance` silmesi artık serbest.
- Varyant tablosu tek satır chip'e indi.
- Etiket ipucu tooltip'e, "1.8 m ref" → "Human scale", font specimen viewer'ın üstüne.

Kalan tek şey: grid'deki font specimen görselinin daha dolu render edilmesi — Adım 4'te
tile işiyle birlikte.

<details>
<summary>Adım 3'ün orijinal kapsamı</summary>

## Adım 3 — Asset sayfası

- Palet: dairesel swatch şeridi, tıkla kopyala, "Detay" içinde yüzdeler + sıralama +
  greyscale + export. Varsayılan kopya formatı `#RRGGBB`.
- Details paneli: ana alanlar üstte, SHA-256 ve tarihler "daha fazla" arkasında.
- Önceki/sonraki asset: `J`/`K` + ok tuşları.
- Asset seviyesinde provenance: küçük combobox (lisans) + kaydet + kaynak linki alanı.
  Bu, `/provenance` sayfasının yerini alıyor (Adım 9'da silinecek).
- Font specimen yukarı taşınır; grid'deki specimen görseli daha dolu render edilir.
- 3D viewer'daki "1.8 m ref" anlaşılır etiketlenir.
- PSD saydamlığı: `decode.go:236` yalnız düzleştirilmiş composite'i okuyor, CraftPix
  PSD'lerinin altındaki opak `Background` katmanı yüzünden sprite beyaz kutuda geliyor.
  Katmanlar okunup en alttaki opak background atlanır.
- Varyant tablosu tek satıra iner.

Dosyalar: `asset.html`, `palette.js`, `app.css`, `derive/decode.go`, `server/*`,
`index/query.go`.

</details>

## [x] Adım 4 — Grid: sayfalama, sıralama, tile (bitti)

Yapıldı: "Load more" yerine numaralı sayfalama (57 sayfa → 9 link, aralık göstergesi,
sayfa boyutu seçici); yedi sıralama, varsayılan "en son eklenen"; API cursor'ı korudu ve
iki mod `Page == 0` ile ayrıldı; sıralama indeksleri (`0016`); tile'lar yeni sekmede,
meta tek satır, checkbox resmin köşesinden çıktı; hover animasyonu artık gerçekten
çalışıyor; seçim sayfalar arası korunuyor + "Select page" + "hepsi" için onay; klavye
(oklar/Enter/Space/n/p); `Ctrl +/-` kaldırıldı.

Ayrıntı: `docs/decisions.md` → "M16 — the grid".

Adım 10'a devredildi: tile üzerindeki hızlı eylemler (indir / yolu kopyala / Godot,
Aseprite, Blender'da aç). Uygulama düğmeleri `ambar://` yardımcısı düzelmeden yarım
çalışacağı için ikisi birlikte yapılacak.

<details>
<summary>Adım 4'ün orijinal kapsamı</summary>

## Adım 4 — Grid: sayfalama, sıralama, tile

- Numaralı sayfalama (`1 2 3 … ‹ ›`), sayfa boyutu 100/200/500. Keyset cursor yerine
  `page` + offset; `Total` zaten hesaplanıyor.
- Sıralama: en son eklenen (varsayılan), dosya tarihi, ad A→Z/Z→A, boyut, tür+ad, piksel
  boyutu. Arama diline `sort:` eklenir. `first_seen_at`, `size`, `mtime` indeksleri.
- Tile'a tıklayınca yeni sekmede açılır.
- Tile bilgisi: ad + piksel boyutu + varyant sayısı.
- Tile hızlı eylemleri: indir, yolu kopyala, etiket ekle, ve Godot/Aseprite/Blender'da aç
  — okunur renklerle ve kendi logolarıyla (logolar Adım 10'daki düzeltmeyle çalışır hale
  gelir).
- Klavye: ok tuşları, Enter, Space=seç.
- `Ctrl +/-` tile boyutu kısayolu kaldırılır (tarayıcı zoom'unu gasp ediyor).
- Hover'da animasyon gerçekten çalışır (`data-anim` şu an ölü kod).
- Seçim sayfalar arası korunur, "tümünü seç" gelir, "or all N" için onay adımı.

Dosyalar: `index/groups.go`, `search/parse.go`, `search/compile.go`, `server/assets.go`,
`assets.html`, yeni `static/grid.js`, `app.css`, migration (indeksler).

</details>

## [x] Adım 5 — Arama (bitti)

Yapıldı: `/api/v1/suggest` gruplu öneri fragment'ı (Filters/Tags/Packs/Files, sayılarla);
`search.js` — klavyeyle gezinme, Tab ile tamamlama, son 8 arama (localStorage), `/` ile
odaklanma, uçuşan isteklerin iptali; kısa placeholder; bare `32x32` ve `dim:`/`px:` ile
piksel boyutu araması; `tris:`/`verts:`/`materials:`/`duration:` gerçek filtre oldu
(önceden sessizce hiçbir şey yapmıyorlardı).

Ayrıntı: `docs/decisions.md` → "M16 — search you can start typing into".

Yapılmayan: model için bbox aralık sorgusu — "2 m küpe sığar mı" kendi sözdizimini
istiyor, üç numerik alan değil.

<details>
<summary>Adım 5'in orijinal kapsamı</summary>

## Adım 5 — Arama

- Öneri endpoint'i: etiket + paket + klasör + dosya adı + `type:`/`has:`/`color:` anahtar
  tamamlama, sonuç sayılarıyla, gruplu.
- Klavyeyle gezinilebilir öneri listesi (`<datalist>` yerine), `/` ile odaklanma, son 8
  arama.
- Piksel boyutu araması: `32x32` gibi bir kısayol ve yaygın sprite boyutları için hızlı
  filtreler.
- Placeholder kısalır.
- 3D için arama hikayesi: tri sayısı, materyal sayısı, bbox aralıkları — mevcut kolonlar
  üstünden filtre olarak açılır.

Dosyalar: yeni `server/suggest.go`, `index/*`, `base.html`, yeni `static/search.js`,
`search/parse.go`.

</details>

## [x] Adım 6 — Upload (bitti)

Yapıldı: sürükle-bırak (tüm sayfa hedef), sıralı kuyruk, `MultipartReader` ile streaming
(sabit bellek, tek yazma, varsayılan sınır yok), gerçek progress bar (yüzde/hız/kalan
süre), arşivin içine bakıp hedef klasör önerisi, tek seviye hedef + yeni klasör, kaynak
linki aynı adımda ve opsiyonel, `ErrDuplicate` zarif ele alındı.

Güvenlik: `/ingest/start`'ta `_inbox/../pack/hero.png` kabul ediliyordu — kütüphane
dosyasını `_quarantine`'e taşıyabilirdi (invariant 1 ihlali). `inboxArchive` önce çözüp
sonra sınırlıyor; testle korundu.

Ayrıntı: `docs/decisions.md` → "M16 — upload that works…".

<details>
<summary>Adım 6'nın orijinal kapsamı</summary>

## Adım 6 — Upload

- Sürükle-bırak, çoklu dosya, kuyruk.
- Hedef klasör seçimi: tek seviye (`2d`, `3d`, `sounds`, `fonts`, `aseprite`, `raw`) +
  yeni klasör yaratma. Karışık arşivde varsayılan `raw`, ama seçim kolay.
- `r.MultipartReader()` ile doğrudan hedefe stream: sabit bellek, tek yazma, `/tmp`
  üzerinden çift yazma biter, LAN'da boyut sınırı kalkar.
- Gerçek progress bar: yüzde, hız, kalan süre; sonra "açılıyor → indeksleniyor → N asset".
- Bitince opsiyonel kaynak linki (`POST /packs/{id}/provenance` zaten var).
- Aynı paketin yeni sürümü yeni klasöre açılır.

Dosyalar: `server/ingest.go`, `ingest/ingest.go`, `ingest.html`, yeni `static/upload.js`,
`config.go`.

</details>

## [x] Adım 7 — Scan ve işler (bitti)

Yapıldı: `/api/v1/jobs/status` + `jobs.js` (2 sn aktif, 30 sn boşta, sekme gizliyken durur);
job ilerleme kolonları (`0017`) + context üzerinden `jobs.Reporter` (400 ms throttle);
scan faz notları; re-scan yerinde çalışıyor, `/jobs`'a yönlendirme yok; `/jobs` tablosunda
ilerleme kolonu ve iş bitince tek seferlik yenileme; junk/trash/dupes'taki "then reload"
cümleleri kalktı; gece 05:00'te tek otomatik tarama (`AMBAR_NIGHTLY_SCAN`, `off` kapatır).

Ayrıntı: `docs/decisions.md` → "M16 — background work you can watch".

<details>
<summary>Adım 7'nin orijinal kapsamı</summary>

## Adım 7 — Scan ve işler

- Re-scan yerinde çalışır, yönlendirme yok; bitince tek satır özet.
- Sol panelde "10 dk önce tarandı · N asset" (`jobs` tablosundan `MAX(finished_at)`).
- Canlı ilerleme: scan/derive/junk/dupe için sayaç.
- `/jobs` iş varken 2 sn'de bir kendini yeniler, boşta yenilenmez.
- "watch background work, then reload" cümleleri gider.
- Gece 05:00–07:00 arası bir kez otomatik scan; başka hiçbir arka plan işi yok.

Dosyalar: `server/derivatives.go`, `index/*`, `jobs/*`, `assets.html`, `jobs.html`,
yeni `static/jobs.js`, `config.go`.

</details>

## [x] Adım 8 — CPU (bitti)

Ölçüldü, sonra düzeltildi. Grid sorgusundaki `palette_json` maliyetin %55'iydi (78 → 35 ms);
`assetListColumns` onu boş string olarak seçiyor. Sol panel agregasyonları istek yolundan
tamamen çıktı (stale-while-revalidate, single-flight, `WithoutCancel`). Filtresiz facet'ler
önbelleğe girdi.

Sonuç (gerçek sunucu): asset sayfası 12–14 ms, aramalı sayfa 10–11 ms, filtresiz grid
74–142 ms (ilk istek 309 ms, snapshot'ı kuruyor). Öncesi ~350 ms/sayfa agregasyon + 112 ms
grid sorgusu.

Ayrıntı: `docs/decisions.md` → "M16 — the CPU, measured before it was optimised".

<details>
<summary>Adım 8'in orijinal kapsamı</summary>

## Adım 8 — CPU

- Sol panel agregasyonları (Stats, Facets, LibraryColours, Tree) 60 sn TTL'li önbelleğe;
  yazmada geçersiz kılınır.
- Sayfalama isteği yalnız tile parçasını render eder (şu an tüm sayfa render edilip
  htmx atıyor).
- `LibraryColours` tam tablo taramasından kurtulur.

Dosyalar: `server/assets.go`, `index/*`, `assets.html`.

</details>

## [x] Adım 9 — Silme listesi (bitti)

Silinen: `/palettes` (handler, şablon, testler, CSS, dört menü linki, yalnız ona hizmet eden
`index/palettes.go` fonksiyonları); palet export'ta `.txt`/`.json`/`.css`/`.png` (exporter'lar
dahil); `/provenance` backlog sayfası ve bulk lisans formu.

Yerine gelen: `has:provenance` arama bayrağı — `-has:provenance` backlog'un kendisi, ama
grid'de, her asset kendi sayfasında düzeltilebilir halde. Sidebar sayacı oraya bağlanıyor.

Kalan (onaylı): `/status`, duplicates, junk, trash, spritesheet onayı, API token sayfası,
font specimen, tarayıcı model thumbnail'ı, "Human scale" referansı.

Ayrıntı: `docs/decisions.md` → "M16 — what was removed, and what replaced it".

- `/palettes` ve ona hizmet eden `index/palettes.go` fonksiyonları.
- Sol paneldeki **Tools** bölümü ve onunla ölen `audition.js`.
- Palet export'ta `.png`, `.txt`, `.json`, `.css` (kalan: `.gpl`, `.gd`, `.tres`).
- `/provenance` sayfası — Adım 3'teki asset seviyesi editör devreye girdikten sonra.

## [x] Adım 10 — `ambar://` yardımcısı ve uygulamada açma (bitti)

Teşhis: script'in kendisi doğruydu. Uygulamanın boş açılması, şemanın kayıtlı olmaması
durumunun görüntüsü — tarayıcı "hangi uygulama?" diye sorup uygulamaya ham `ambar://`
URL'ini veriyor. Kayıt tutmuyordu çünkü kurulum sıraya bağlı iki adımdı ve desktop girdisi
script'in o anki yerini kaydediyordu.

Yapıldı: `--install` kendini `~/.local/bin/ambar-open`'a kopyalayıp onu kaydediyor ve
doğruluyor; `--check` ve `--test`; uygulama çözümü binary → Flatpak → xdg-open, üzerine
`~/.config/ambar-open.conf`; olmayan dosyada sessizce başarılı olmak yerine hata; `%20`
kodlaması (gerçek `+` içeren dosya adları artık bozulmuyor); tile'larda ve asset sayfasında
her uygulamanın kendi rengiyle okunur düğmeler + indir + yolu kopyala.

Ayrıntı: `docs/decisions.md` → "M16 — why 'open in Aseprite' opened Aseprite with nothing in it".

Bug: yardımcı uygulamayı açıyor ama dosyayı vermiyor. Şema çözümlemesi, yol aktarımı ve
uygulama argümanları elden geçer; Godot/Aseprite/Blender için logolu, okunur düğmeler.

Dosyalar: `server/openhelper.go`, `server/openwith.go`, `openwith.js`, `asset.html`,
`assets.html`.

## [x] Adım 11 — Godot eklentisi (bitti, Godot 4.7.1'de doğrulandı)

Teşhis: dört sebep. `EditorInterface` singleton'ı (4.2+) 4.1'de script'i yükletmiyor; dock
yanlış yerdeydi ve zaten "2D/3D yanındaki menü" demek main screen plugin demek; ayarlar
Editor Settings'te kimsenin bulamayacağı yerdeydi ve varsayılan URL kimsenin ağında çözülmüyor;
ve bağlantı testi `/api/v1/healthz`'e gidiyordu — o session auth'lu, geçerli token'a 401 diyor.

Yapıldı: main screen sekmesi, kendi içinde kurulum paneli + bağlantı testi, `res://ambar.cfg`
(URL, commit edilir) / `user://ambar_token.cfg` (token, edilmez) ayrımı, thumbnail'lı gezinme,
import + manifest + sunucuya kayıt, credits, pixel-art import varsayılanları, sürüm uyumluluk
katmanı, `/api/v1/ping`, README.

Asıl kök sebep, çalıştırınca çıktı ve yukarıdaki dördü de değildi: `project.gd`'de
`var data := _read_json(...)` — tipsiz dönüşten çıkarım. GDScript bunu reddediyor, bir
dosyadaki *parse* hatası onu preload eden her şeyin derlemesini düşürüyor, ve derlenmeyen bir
eklenti "etkin" görünüp hiçbir şey yapmıyor. Dört `:=` → `=`.

Çalıştırınca iki hatam daha çıktı: main screen'e giden kontrol için `remove_control_from_docks`,
ve kontrol ağaca eklenmeden önce ilk aramanın atılması (HTTPRequest ağaç dışında çalışmıyor).

Doğrulandı (Godot 4.7.1, headless): editör yüklemesi hatasız; config round-trip; ping; search
249 sonuç; thumbnail 61 KB → 385x512 Image; yanlış token'da anlaşılır mesaj; import →
`res://assets/image/<pack>/1.png` 1798 bayt; manifest; credits.md; ve asset sayfasında
"AmbarPluginTest · res://assets/…".

Ayrıntı: `docs/decisions.md` → "M16 — the Godot plugin, which did nothing".

Şu an kurulunca hiçbir şey olmuyor. Hedef: editör üst menüsünde "Ambar", API token + URL
girişi, kütüphaneyi dock'ta gezme, tek tıkla import, kullanım kaydı.

Dosyalar: `addons/ambar/*`, gerekirse `server/api*.go`.

## [ ] Adım 12 (ertelendi — "unreal i sonra yapariz") — Unreal eklentisi

Sıfırdan. Kapsamı Adım 11 bittikten sonra netleşir.

## [x] Adım 13 — Spec, karar kaydı, kapanış

`docs/spec.md` güncellendi: **§8** tamamen yeniden yazıldı (var olan anlatılıyor, tersine
çevrilen kararlar gerekçesiyle duruyor, yapılmayanlar "ertelendi" diye işaretli); **§10**
Godot bölümü doğrulanmış hâline göre düzeltildi (`.import` yazmak yerine import
varsayılanları, dock yerine main screen, Editor Settings yerine iki config dosyası) ve
headless test paragrafı eklendi; **§12**'ye gecelik tek zamanlanmış iş ve "boşta boşta
demektir" CPU kuralı; **§13** gerçek env listesiyle hizalandı (upload sınırı 0 = sınırsız,
`AMBAR_NIGHTLY_SCAN`, `AMBAR_LOCAL_LIBRARY_PATH`); **§14**'e M14/M15/M16 satırları;
**§15** "yazılmadan önce karara bağlanacaklar"dan "karara bağlandı" kaydına dönüştü;
**§16**'ya iki kural (önce ölç; Go dışında çalışan her şey kendi runtime'ında çalıştırılır);
**§17** gerçek klasör düzeni, upload'ın hedef seçimi ve `_inbox` sınırı; **§0**'a arayüzün
ürünün parçası olduğu maddesi.

`docs/ui-review-questions.md` → `docs/archive/2026-07-ui-review-questions.md`.

---

## Açık kalan tek karar

`raw/` çifte görev yapıyor: içinde eski indirilmiş zip'ler var, ama karışık arşivlerin
açılma hedefi de o olacak. Önerilen: zip'ler `_raw/`'a taşınır (alt çizgi indeksten
otomatik çıkarır), `raw/` karışık paketlere kalır. Onay bekliyor; Adım 6'ya kadar zamanı
var.
