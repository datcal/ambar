# M17 — 3D, hover animasyonu, renkler

Kaynak: 2026-08-01 geri bildirimi. Hepsi `testdata/data/ambar.db` (gerçek kütüphanenin
11.839 varlıklık kopyası) üzerinde ölçüldü; aşağıdaki sayılar tahmin değil.

---

## [x] 1. gltf açılıyor ama içi boş

**Ölçüm.** 442 `.gltf` varlığın hepsi `derive_state=ok`. Üretilen `preview.glb` **1.396 bayt**
ve içinde geometri yok:

    buffers: [{"uri": "building_archeryrange_blue.bin", "byteLength": 202372}]
    images:  [{"uri": "hexagons_medieval.png"}]

`gltf.SaveBinary` harici buffer URI'sini olduğu gibi bırakmış, yani "GLB" adı taşıyan bir
dosya hâlâ yanındaki `.bin`'e bakıyor. Tarayıcı `/assets/4988/preview.glb`'yi yükleyince
buffer'ı `/assets/4988/building_archeryrange_blue.bin` diye istiyor — böyle bir rota yok
(companion rotası `/assets/{id}/file/{ad}`), 404, sahne boş kalıyor. Küçük resim var çünkü o
başka yoldan (tarayıcı render'ı) üretilmiş; boş olan sadece görüntüleyici.

**Yapılacak.** `internal/model.Normalize` buffer'ı ve görselleri GLB'nin içine gömecek
(`doc.Buffers[0].URI` temizlenince `SaveBinary` BIN chunk'ı olarak yazar; görseller
bufferView'a alınır). Böylece `preview.glb` gerçekten kendi kendine yeten bir dosya olur —
API'yi ve Godot eklentisini kullanan herkes için de doğrusu bu. 442 gltf'i yeniden türetmek
için `derive_state` sıfırlayan bir migration (0015'in PSD için yaptığının aynısı).

## [—] 2. "gltf'i hiç gösterme, fbx'i göster" — yapılmadı

**Ölçüm.** 968 model grubunun primary dağılımı:

| primary | grup | durum |
| --- | --- | --- |
| gltf | 442 | hepsi çok formatlı (fbx + obj kardeşi var), **tek başına gltf olan grup yok** |
| fbx | 244 | `needs_blender` — **görüntü yok**, hepsi yalnızca fbx |
| fbx | 198 | ok |
| obj | 84 | ok |

**Katılmadığım yer.** İstenen sıralama (fbx > gltf) bu kütüphanede durumu **kötüleştirir**:
fbx'in önizlemesi Blender'a bağlı ve Blender kurulu değil — hâlihazırda görüntüsüz 244 grubun
sebebi tam olarak bu. gltf ise saf Go ile türetiliyor ve tarayıcıda doğrudan açılıyor. Yani
gltf'i primary'den düşürmek, çalışan 442 önizlemeyi Blender bekleyen 442 önizlemeye çevirir.

Ayrıca endişelendiğin durum ("tek başına gltf olursa ne yaparız") bu kütüphanede hiç yok:
442'nin hepsinin fbx ve obj kardeşi var.

**Önerim.** 1. maddeyi yap, gltf düzgün açılsın; sıralama olduğu gibi kalsın. Buna rağmen
fbx'i istersen sıralamayı `glb > fbx > obj > gltf` yaparım — ama o zaman 442 grubun küçük
resmi de tarayıcı render'ına kalır. **Karar senin.**

## [x] 3. Animasyonu olmayan resim hover'da bozuluyor

**Ölçüm.** Bu en büyüğü.

    animasyonlu sayılan varlık (frame_count > 1)   6.706
    gerçekten var olan anim.gif                      795
    sheet.gif (tespit edilmiş grid önizlemesi)     1.302

`Animated()` = `frame_count > 1`. Ama `frame_count`'un 5.905'i **tahmin** — spritesheet grid
tespiti. Örnekler:

    towers_walls_grass_dark_1.png   48x40 grid  =  1920 "kare"
    body_tracks.png                  8x8  grid  =    64 "kare"

Bunlar tileset, animasyon değil. Sonuç: ~5.900 tile'da `data-anim="/assets/N/anim.gif"` var,
dosya yok, hover 404 yüklüyor ve `<img>` boşalıyor. Tarif ettiğin şeyin tamamı bu.

**Yapılacak.**

* `Animated()` yalnızca **gerçek** animasyon demek olacak: kaynak dosyanın kendisi animasyonlu
  (`frame_source` boş → 795) ya da gridi bir insan onaylamış (`frame_source='manual'` → 6).
  Tahmin edilmiş grid hover'da oynamaz — §6 zaten "bir tahmin sessizce güvenilir sayılmaz"
  diyor, onay ekranı bunun için var.
* Tile var olan dosyaya işaret edecek: gerçek animasyon `anim.gif`, onaylanmış grid `sheet.gif`.
* `grid.js`'e `onerror`: yükleme başarısızsa hareketsiz resme geri dön. Bir daha aynı şekilde
  bozulmasın diye.
* Detay sayfasındaki Play düğmesi de aynı koşula bağlanacak.

**Ayrıca bildirmem gereken şey:** 1920 kareli tespit sadece hover'ı değil, detay sayfasında
yazan kare sayısını da yanlış gösteriyor ve Godot'a giden grid bilgisini de etkiliyor. Tespit
eşiklerini gevşetmek ayrı bir iş; bu adımda sadece hover'ı ve gösterimi düzeltiyorum, tespiti
yeniden ayarlamak istersen ayrıca söyle.

## [x] 4. Soldaki renkler çok az

**Ölçüm.** `LibraryColours(ctx, 18)` — sınır 18. Kütüphanede 0.02 eşiğinin üstünde **673**
renk kovası var; ilk 40'a bakınca yeşiller, sarılar, maviler ancak 20. sıradan sonra geliyor.
Yani 18 sınırı listeyi koyu kahve/griye hapsediyor.

**Yapılacak.** 40'a çıkar. SQL maliyeti değişmez (gruplama zaten tamamı üzerinde, `LIMIT`
sadece kırpıyor), 40 daire de yer olarak sorun değil.

## [x] 5. Çoğu modelin görüntüsü yok

**Ölçüm.** 244 grup `fbx`, `needs_blender`, ve bunların hiçbirinin obj/gltf kardeşi yok — yani
sıralamayla kurtarılamazlar. Tarayıcı render'ı bunları hallediyor **ama sayfa yüklemesi başına
`BUDGET = 12`**. 100 tile'lık bir sayfanın dolması için ~20 ziyaret gerekiyor.

**Yapılacak.** Sayfa başına sabit bütçeyi kaldır: görünür alana giren her model tile'ı sırayla
render edilsin (yine teker teker, yine tek WebGL bağlamı, yine boşta zamanlama). Bir gezinmede
bitsin, yirmi ziyarette değil.

---

## Sıra

1. Renk sınırı 40 (tek satır, hemen görünür)
2. Hover animasyonu (en çok tile'ı etkileyen bozukluk)
3. gltf gömme + migration + yeniden türetme
4. Model küçük resim bütçesi
5. (karar bekliyor) format sıralaması

---

## Sonuç (2026-08-01, çalışan sunucuda doğrulandı)

    preview.glb (gerçek dosya)   1.396 -> 224.780 bayt, kaynak silinince bile okunuyor
    gltf görüntüleyici           orijinal + .bin (202.372) + doku (15.783), üçü de 200
    hover önizlemesi             36/100 tile teklif ediyor, 36'sı da 200
                                 (öncesi: 100/100 teklif ediyordu, örneklenenlerin hepsi 404)
    kenar çubuğu renkleri        18 -> 40 benzersiz renk
    model küçük resmi            sayfa başına 12 sınırı kaldırıldı; asıl sebep ise
                                 gltf/obj tile'larının derive_state='ok' yüzünden
                                 olmayan bir thumb'a <img> basmasıydı (254 tile).
                                 ?kind=model: 29 resim (29'u da 200), 71 tanesi
                                 tarayıcıya kuyruklandı — 71'i de eskiden boştu
    migration 0018               442 gltf yeniden türetildi; 221+6 takılı satır onarıldı

Testler: `internal/model` (gömme + yol güvenliği), `internal/index` (Animated/AnimatedPreview
tablo testi), `internal/server` (grid işaretlemesi, görüntüleyici kaynağı). `make check` temiz.

2. madde yapılmadı: ölçüm, fbx'i öne almanın çalışan 442 önizlemeyi Blender bekleyen 442
önizlemeye çevireceğini gösterdi. Gerekçe `docs/decisions.md`'de; düzelmiş görüntüleyiciyle
bakıp yine de istersen tek satırlık değişiklik.
