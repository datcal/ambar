# UI revizyonu — açık sorular (arşiv, 2026-07)

> **Arşiv.** Bu dosya M16'nın girdisiydi: 2026-07-31 gözden geçirmesinin soruları ve
> altlarındaki cevaplar. Cevapların hepsi uygulandı; nedenleriyle birlikte
> `docs/decisions.md`'nin M16 bölümlerinde, planı `docs/m16-plan.md`'de duruyor.
> Burada değişiklik yapmayın — kaydın kendisi.

Cevapları soruların altındaki **Cevap:** satırına yaz; boş bıraktıklarında yanındaki
öneriyle devam ederim.

İşaretler:

- **[BLOK]** — cevap gelmeden o işe başlayamam.
- **[varsayılan]** — boş bırakırsan yazdığım öneriyle giderim.

---

## 0. Zaten karara bağlandı sayıyorum

Bunlar konuşmada netleşti; yanlış anladığım varsa üstünü çiz.

1. `/palettes` (pack palet karşılaştırma) silinecek. `color:` araması, sol paneldeki
   renk filtresi ve asset paletindeki her şey kalıyor.
2. Sol paneldeki **Tools** bölümü silinecek (`assets.html:152-168`). Bu, klavye
   audition'ının tek giriş noktası olduğu için `audition.js` de ölüyor.
3. "Load more" gidiyor; yerine numaralı sayfalama (`1 2 3 … ‹ ›`), ilk sayfa 100 kayıt.
4. Üstteki arama kutusuna gerçek autocomplete gelecek.
5. Anasayfadaki **Re-scan** artık `/jobs`'a yönlendirmeyecek, yerinde çalışacak; altında
   "10 dk önce tarandı" bilgisi olacak.
6. Grid'e sıralama seçenekleri gelecek.
7. Upload sürükle-bırak olacak, hedef klasör sorulacak, gerçek progress bar olacak,
   kaynak linki sonradan ve opsiyonel sorulacak.
8. 2D viewer'da normal tekerlek sayfayı kaydıracak; zoom ayrı bir yolla olacak (bkz. 34).

---

## 1. Silme listesi

Her biri hem kod hem menü yüzeyi eksiltiyor. "Kalsın" dersen hiç dokunmam.

### 1.1 `/status` sayfası **[varsayılan: sil]**
`index.html`, 35 satır + topnav linki. İçeriği "Signed in as burak" + iki tablo; sayılar
zaten sol panelde var.

**Cevap:**
/status sayfasi dursun

### 1.2 `ambar://` açma yardımcısı **[BLOK]**
`openhelper.go` (370) + `openwith.go` (130) + `openwith.js` (55) + `/settings/open-helper`
sayfası + asset sayfasında iki paragraf. "Aseprite'ta aç" düğmelerinin çalışması için her
makineye tek seferlik bir yardımcı kurmak gerekiyor. Kurdunuz mu, kullanıyor musunuz?
Kullanmıyorsanız "yolu kopyala" düğmesi zaten işi görüyor.

**Cevap:**
bu dursun ama duzgun calismiyor. yukledim tiklayinca ambar ile aciyor sonra hangi uygulama oldugunu soyleyince bos olarak o uygulamayi aciyor. bunun cozulmesi lazim

### 1.3 Font specimen **[BLOK]**
`specimen.js` (71) + `asset.html:246-275` + `/assets/{id}/font` rotası. Asset sayfasında
kendi yazını font'ta deneme kutusu. `fonts/` klasörünüz var — bunu kullanıyor musunuz?

**Cevap:**
evet kullaniyorum o cok guzel sadece font deneme ekranini yukari alirdim. font sayfasina koydugun image biraz bos

### 1.4 Palet export formatları **[varsayılan: `.gpl`, `.gd`, `.tres` kalsın; `.png`, `.txt`, `.json`, `.css` gitsin]**
Şu an 7 format da asset sayfasında tek satırda linkli duruyor.

**Cevap:**
okay

### 1.5 `/provenance` backlog sayfası **[BLOK]**
`provenance.go` (235) + `pack_provenance.html` (92). "Hangi paketin lisansı belirsiz"
listesi. Oyunu ticari dağıtacaksanız atıf dosyası için gerekli; dağıtmayacaksanız defter
tutmak. Ne olsun?

**Cevap:**
http://meshnas.local:8973/provenance bu sayfada gitsin cunku her zaman folder folder gelmeyecekler bazen sadece 1 tane png iniyor.  asset detay sayfasinda ufak bi combobox ve button ile yapabiliriz ve link ekleme yeri de olsun

### 1.6 3D viewer'da "1.8 m ref" **[varsayılan: kalsın]**
İnsan boyu referans küpü. Model ölçeği kontrolü için ucuz bir araç.

**Cevap:**
ref guzel ama 1.8 nedir anlamadim. 

### 1.7 Tarayıcıda model thumbnail'ı **[varsayılan: kalsın]**
`modelthumb.js` (297) + `modelthumb.go` (223). Sunucuda 3D renderer yok, Blender opsiyonel;
bu yüzden `.obj`/`.fbx` küçük resmini tarayıcı üretip sunucuya geri yüklüyor. Alternatifi
NAS'a Blender kurmak. Kalsın mı?

**Cevap:**
kalsin 

### 1.8 Bunlar kalıyor, itirazın varsa söyle
`duplicates`, `junk`, `trash`, spritesheet grid onayı (Godot `AnimatedSprite2D` importu
buna bağlı), API token sayfası (Godot eklentisi buna bağlı).

**Cevap:**
kalsin

---

## 2. Upload ve klasör düzeni

Mevcut düzen: `2d 3d _archives _inbox aseprite fonts raw sounds`

### 2.1 Karışık arşiv **[BLOK]**
Bir zip'in içinde hem sprite hem ses varsa hedefi nasıl seçelim? (Arşivi açmadan içeriğini
sayabiliyoruz.)


**Cevap:**

`raw/` gibi bir "karışık" klasörünü varsayılan yap, ama secim kolay olsun 2d veya baska bir sey secebileyim ve yaratabileyim yeni folder

### 2.2 İkinci seviye klasör **[BLOK]**
`2d/kenney-platformer` yeter mi, yoksa `2d/tiles/`, `2d/characters/` gibi alt kırılım da
seçebilmek ister misin? (Ağaç destekliyor; upload son seçimi hatırlar.)

**Cevap:**
sadece 2d olsun

### 2.3 `_inbox/` yolu **[varsayılan: kalsın]**
Web upload düzeldikten sonra SMB üzerinden `_inbox/`'a atma yolu kalsın mı, yoksa tek yol
tarayıcı mı olsun? (Kalması 5 GB'lık paketler için hâlâ en sağlam yol.)

**Cevap:**
kalsin

### 2.4 `raw/` klasörü **[BLOK]**
Şu an indeksleniyor: `library/walk.go:210` yalnızca alt çizgiyle başlayan üst düzey
klasörleri atlıyor (`_inbox`, `_archives`, `_quarantine`). İçinde ne var?

- (a) Ham indirmeler/zip'ler → adını `_raw` yap, indeksten otomatik çıkar
- (b) Gerçek asset'ler → olduğu gibi kalsın

**Cevap:**

raw klasorunun icinde benim daha onceden indirdigim assetslerin zip dosyalari var. bir sey olmasin diye orada tutuyorum. cok onemli degil . 

### 2.5 `_archives/` **[varsayılan: saklamaya devam]**
İçe alınan orijinal zip'ler burada tutuluyor (`KeepArchives`). Disk maliyeti kütüphane
kadar; karşılığı, bir paketi sıfırdan yeniden açabilmek. Devam mı, silinsin mi?

**Cevap:**

### 2.6 Upload boyut sınırı **[varsayılan: 0 = sınırsız (LAN)]**
Streaming'e geçince 100 MB duvarına gerek kalmıyor. LAN'da sınırsız mı olsun, yoksa bir
tavan (ör. 5 GB) mı kalsın?

**Cevap:**
lan da sinirsiz

### 2.7 Aynı paketin yeni sürümü **[varsayılan: yeni klasör + "önceki sürüm" bağı]**
`kenney_platformer_v2.zip` bıraktın. Yeni klasör mü açsın, var olan paketin üstüne mi
yazsın (invariant 1 gereği üstüne yazmak riskli), yoksa "bu şu paketin yeni sürümü" diye
sorup ilişkilendirsin mi?

**Cevap:**
yeni folder acsin

### 2.8 Kaynak linki hatırlatması **[varsayılan: evet]**
Link vermezsen paket `needs_provenance` durumunda kalıyor. Sol panelde "3 paketin kaynağı
belirsiz" gibi sessiz bir hatırlatma olsun mu, yoksa hiç mi görünmesin?

**Cevap:**

---

## 3. Grid, sayfalama, sıralama

### 3.1 Varsayılan sıralama **[varsayılan: en son eklenen]**
Şu an tek sıra var: dosya adı A→Z, tüm kütüphane karışık (`groups.go:360`).

**Cevap:**

### 3.2 Sıralama seçenekleri **[varsayılan: aşağıdaki altısı]**
En son eklenen · dosya tarihi (mtime) · ad A→Z / Z→A · boyut · tür sonra ad · piksel
boyutu. Eklemek/çıkarmak istediğin?

**Cevap:**

### 3.3 Sayfa boyutu **[varsayılan: 100, seçenekler 100/200/500]**

**Cevap:**

### 3.4 Klasör gruplu görünüm **[varsayılan: hayır, düz liste]**
`2d/` altındakiler bir arada, başlıkla ayrılmış bir görünüm ister misin, yoksa seçtiğin
sıralamaya göre düz liste yeter mi?

**Cevap:**

### 3.5 Tile'da hangi bilgi **[varsayılan: ad + boyutlar + varyant sayısı]**
Şu an: ad, pack adı, dosya boyutu, piksel boyutu, badge'ler. Küçük tile boyutlarında
okunmuyor. Ne kalsın?

**Cevap:**

### 3.6 Tıklayınca ne olsun **[BLOK]**
- (a) Şimdiki gibi tam sayfa asset görünümü
- (b) Sağ panelde önizleme (Eagle/Bridge gibi), tam sayfa için ikinci tık ← **önerim**
  (NAS'ta her tıkta tam sayfa yüklemesi pahalı)

**Cevap:**
yeni sekmede ac asset i 

### 3.7 Tile hızlı eylemleri **[varsayılan: indir + yolu kopyala + etiket ekle]**
Üstüne gelince çıkan küçük düğmeler. Hangileri işine yarar? (Godot'a gönder de mümkün ama
eklenti tarafını değiştirmek gerekir.)

**Cevap:**
godot ve asprite ve blender kesinlikle olmali ve calismali su anda calismiyor. ve renkleri okunmuyor. onlarin okunur ve kendi logolari ile falan olmasi lazim

### 3.8 `Ctrl +/-` kısayolu **[varsayılan: kaldır]**
Şu an grid'de tarayıcı zoom'unu gasp edip tile boyutunu değiştiriyor
(`workspace.js:53-66`) ve bu hiçbir yerde yazmıyor. Kaldıralım mı, kalsın mı?

**Cevap:**

### 3.9 Klavye ile gezinme **[varsayılan: ok tuşları + Enter + Space=seç]**
Grid'de hiç klavye yok. İstediğin tuş düzeni var mı?

**Cevap:**
olur guzel olur
---

## 4. Arama

### 4.1 Autocomplete neyi önersin **[varsayılan: hepsi, gruplu]**
Etiketler · paket adları · klasörler · dosya adları · `type:`/`has:`/`color:` gibi anahtar
tamamlama. Her satırda sonuç sayısı. Çıkarmak istediğin var mı?

**Cevap:**

### 4.2 Son aramalar **[varsayılan: evet, son 8]**
Kutuya odaklanınca son aramaların çıksın mı? (Kayıtlı aramalar ayrı kalıyor.)

**Cevap:**

### 4.3 `/` kısayolu **[varsayılan: evet]**
Herhangi bir sayfada `/` ile arama kutusuna odaklan.

**Cevap:**

### 4.4 Yazım hatası toleransı **[varsayılan: hayır, önce prefix eşleşmesi]**
`swrod` → `sword` bulsun mu? SQLite tarafında maliyetli; NAS'ta düşünmek lazım. Şimdilik
"öneri listesi yeter" mi?

**Cevap:**


32x32 gibi pixel boyutuna gore arama isterim. 2d lerde


3d modeller icin nasil arama yapariz bilmiyorum. 
---

## 5. Asset sayfası

### 5.1 Palet: kaç renk **[varsayılan: ilk 16 daire, gerisi Detay'da]**

**Cevap:**

### 5.2 Kopyalama varsayılan formatı **[varsayılan: `#RRGGBB`]**
Alternatif: Godot `Color(r, g, b)`. Hangisi günlük işine daha yakın?

**Cevap:**

### 5.3 Details panelinde ne kalsın **[varsayılan: aşağıdaki gibi]**
Ana: yol, pack, tür, boyut, piksel boyutu, renk sayısı, alfa.
"Daha fazla" arkasında: SHA-256, ilk indeksleme, son doğrulama, uzantı, değişiklik tarihi.

**Cevap:**

### 5.4 Önceki/sonraki asset **[varsayılan: `J`/`K` + ok tuşları, ikisi de]**

**Cevap:**

### 5.5 Hızlı etiketleme **[BLOK]**
Tek oturumda 50 asset etiketleyeceksen şimdiki serbest metin kutusu yavaş. En çok
kullandığın etiketler için tek tık düğme paleti ister misin? İsterse hangileri sabit
olsun (ör. `theme:`, `style:`, `license:` altındakiler)?

**Cevap:**
evet guzel olur bir kac tane daha koy size falan gibi 32x32 diyebilmek icin 

### 5.6 Lisans asset seviyesinde **[varsayılan: hayır, pack seviyesi yeter]**
Şu an lisans/kaynak pack'e bağlı. Tek tek asset'e lisans girmek gerekiyor mu?

**Cevap:**

---

## 6. 2D viewer ve pixel art

### 6.1 Zoom nasıl olsun **[BLOK]**
Normal tekerlek sayfayı kaydıracak. Zoom için:

- (a) Sadece `Ctrl/⌘ + tekerlek`
- (b) Toolbar'da hatırlanan bir "tekerlek: zoom / kaydır" anahtarı
- (c) İkisi birden ← **önerim** (anahtar kapalıyken `Ctrl+tekerlek` yine zoom yapar)

**Cevap:**
C
### 6.2 Bulanıklık teşhisi **[BLOK]**
Bulanık gördüğün bir asset'i aç, sağ panelde **Details** altına bak:

- "Colours" satırının yanında **"pixel art" badge'i var mı?**
- Renk sayısı kaç?
- "Dimensions" kaç?

Badge yoksa tespit eşiklerini kalibre etmem gerekiyor; varsa sorun preview
çözünürlüğünde. İki üç farklı dosya için yazarsan daha iyi.

**Cevap:**

bunu coz ya 
/mnt/game-assets/2d/craftpix-net-189780-free-top-down-pixel-art-guild-hall-asset-pack/PSD/Citizen1/Idle/Citizen1_Idle_front.psd 
/mnt/game-assets/2d/craftpix-net-189780-free-top-down-pixel-art-guild-hall-asset-pack/ASEPRITE/Citizen1/Idle/Citizen1_Idle_front.aseprite 

### 6.3 Varsayılan render **[varsayılan: pixels (smoothing kapalı)]**
Viewer keskin başlasın, "Smooth" düğmesiyle yumuşatılsın. Fotografik texture'lar da keskin
başlar — sorun olur mu?

**Cevap:**

### 6.4 Preview çözünürlük tavanı **[varsayılan: pixel art için kaldır]**
Şu an 2048px (`thumbnail.go:61`), yani 4096'lık atlas yarıya iniyor. Kaldırmanın maliyeti
disk: düz paletli sanat lossless WebP'de küçük sıkışıyor, fotografik texture'da sıkışmıyor.
Pixel art için kaldırıp diğerlerinde 2048'de bırakmak mantıklı mı?

**Cevap:**

### 6.5 Zoom kademeleri **[varsayılan: 1x 2x 4x 8x + tam sayı kesirler]**
Aseprite gibi küçültmede de 1/2, 1/3 gibi tam sayı kesirlere oturalım mı?

**Cevap:**

---

## 7. Scan, işler, CPU

### 7.1 Re-scan bitince ne göstersin **[varsayılan: tek satır özet]**
Örnek: "Tarama bitti — 12 yeni, 3 eksik, 8 değişmiş. Grid'i yenile."

**Cevap:**

### 7.2 Zamanlanmış otomatik scan **[BLOK]**
Gece 03:00 gibi bir saatte kendi kendine taransın mı? (Silme değil, sadece indeksleme —
invariant 3'ü ihlal etmiyor. Ama NAS'ta CPU maliyeti var.)

**Cevap:**
gece super olur sabaha karsi 5 6 7 arasi. surekli arkada bir sey calismasin. ben basarim scan dugmesine scan lazimsa

### 7.3 `/jobs` yenileme aralığı **[varsayılan: iş varken 2 sn, boşta yok]**

**Cevap:**

### 7.4 `AMBAR_WORKERS` **[varsayılan: 2 (şimdiki)]**
NAS'ın kaç çekirdeği var, başka ne çalışıyor? Derive kuyruğunu 1'e çekmek ingest'i
yavaşlatır ama NAS'ı rahat bırakır.

**Cevap:**

### 7.5 Sol panel önbelleği **[varsayılan: 60 sn TTL + yazmada geçersiz kılma]**
NAS'taki CPU'nun ana sebebi: her sayfa yüklemesinde 5 tam tablo taraması. Alternatif,
scan sonunda yazılan bir özet tablo (daha karmaşık, ama her zaman anında). Hangisi?

**Cevap:**

---

## 8. Görsel dil

### 8.1 Tema **[varsayılan: koyu kalsın]**
Açık tema gerekli mi? (İki tema iki kat CSS bakımı.)

**Cevap:**

### 8.2 Accent rengi **[varsayılan: linkler ve aktif durum için ayrı tonlar]**
Şu an tek mavi (`#6ea8fe`) her şeyde: link, buton, aktif sekme, odak, vurgu. Marka rengi
tercihin var mı?

**Cevap:**

### 8.3 Yıkıcı eylemler **[varsayılan: kırmızı + yazarak onay]**
`trash` purge Ambar'daki tek geri dönüşsüz işlem ve şu an normal bir düğme gibi duruyor.
Onay için "purge" yazdırma çok mu?

**Cevap:**

### 8.4 Yoğunluk **[varsayılan: daha sıkı]**
Satır yükseklikleri ve boşluklar daralsın, yazı biraz büyüsün ve kontrast artsın (şu an her
şey 0.8-0.9rem gri). İtirazın var mı?

**Cevap:**

### 8.5 Açıklama yazıları nereye **[varsayılan: tooltip + settings]**
Kalıcı paragraflar gidiyor. Yerine: kısa tooltip'ler, kurulum bilgileri `/settings`'te,
boş durumlarda tek satır. Bir de "yardım" sayfası ister misin, yoksa hiç mi?

**Cevap:**

---

## 9. Süreç

### 9.1 Sıra **[varsayılan: aşağıdaki]**
1. Viewer/pixel-perfect + palet daireleri (senin ilk şikayetlerin)
2. Silme listesi (`/palettes`, Tools, onayladıkların)
3. Sayfalama + sıralama
4. Upload
5. Autocomplete
6. Scan akışı
7. CPU
8. Menü + görsel dil
9. Kalan ölü işler (hover animasyonu, grid klavyesi, tile eylemleri)

**Cevap:**

### 9.2 Commit büyüklüğü **[varsayılan: her paket ayrı commit]**
Tek büyük "M16" commit'i mi, paket paket mi? (Commit'i sen atıyorsun; ben sadece çalışmayı
bırakırım.)

**Cevap:**

### 9.3 Test beklentisi **[varsayılan: mevcut testler kırılmasın + yeni sunucu tarafı testler]**
`CLAUDE.md`'deki zorunlu test listesi UI içermiyor. Sayfalama, sıralama ve upload hedef
klasörü için sunucu tarafı test yazacağım. JS adaları için test altyapısı yok — kurmak
ister misin, yoksa elle mi doğrularız?

**Cevap:**

### 9.4 Godot eklentisi **[BLOK]**
`addons/ambar` dock'u da bu gözle gözden geçirilsin mi, yoksa şimdilik web arayüzü mü?

**Cevap:**
bu addons u yukledim hic bir sey olmadi. yukariya 2d 3d gibi godot un menusunun orada Ambar diye menu gelecek ben API token ve url falan gircem sonra her seyi orada gorecegim. import edecegim vs vs saniyordum hic birisi olmadi/ ve bunu unreal icin de yapmak gerekir. 

### 9.5 Spec ve karar kaydı
Bunların çoğu `docs/spec.md` §8'den kasıtlı sapma. `docs/decisions.md`'ye "M16 — arayüz
revizyonu" başlığı altında, her sapma için tek satır gerekçeyle yazacağım. Spec'in kendisi
güncellensin mi, yoksa sapmalar sadece kararlar dosyasında mı kalsın?

**Cevap:**
guncelle ya o bence eski kalmis. biraz kisiklamalari kaldiralim 
---

## 10. Senin soruların / notların

Buraya ekle, aşağıya yazacağın her şeyi okuyorum.

ya genel olarak cok eski hissettiriyor. hic modern degil tasarim .