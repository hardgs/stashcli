# stashcli — تخزين سحابي باستخدام تيليجرام

خزّن، زامِن، وابثّ ملفاتك باستخدام حساب تيليجرام الخاص بك كواجهة تخزين
خلفية. يمكنك تركيبه كقرص FTP أو WebDAV، أو استخدامه مباشرة من سطر
الأوامر — يتم تقسيم الملفات إلى أجزاء مشفّرة ورفعها كرسائل تيليجرام، ثم
إعادة تجميعها عند التنزيل.

- **الملفات الكبيرة تبقى في أقل عدد ممكن من الأجزاء.** افتراضيًا، يُرفع
  كل ملف على شكل أجزاء ثابتة حجمها **450 ميغابايت** — فيلم بحجم 80
  ميغابايت يُرفع في **رسالة واحدة** فقط، بدلاً من عشرات الرسائل الصغيرة.
- **سريع افتراضيًا.** الرفع والتنزيل يستخدمان **4 أجزاء بالتوازي** دون
  الحاجة لأي إعداد إضافي.
- **قابل للاستئناف.** انقطاع الاتصال أو إغلاق البرنامج فجأة لا يُفقدك
  عملية النقل بأكملها — أعد تشغيل نفس الأمر وسيكمل من حيث توقف.
- **الملف لا يظهر إلا بعد اكتمال رفعه بالكامل.** لا تظهر أي إدخالات
  ناقصة/غير مكتملة في `ls` أو FTP أو WebDAV أثناء النقل.
- **يعمل على عدة أنظمة تشغيل.** يعمل على Windows وLinux وmacOS من ملف
  تنفيذي واحد بسيط.

---

## 1. التثبيت

تحتاج إلى تثبيت [Go 1.21 أو أحدث](https://go.dev/dl/). هذا هو المتطلب
الوحيد للبناء.

```bash
git clone https://github.com/yourname/stashcli.git
cd stashcli
go build -o stashcli .
```

هذا كل شيء — أصبح لديك الآن ملف تنفيذي باسم `stashcli` (أو
`stashcli.exe` على Windows) في المجلد الحالي. لا حاجة لـ Docker أو أي
خدمة خارجية أخرى.

### البناء لنظام تشغيل مختلف (Cross-compiling)

يمكن لـ Go بناء ملفات تنفيذية لأنظمة أخرى دون الحاجة لتثبيتها:

```bash
# Windows (من Linux/macOS)
GOOS=windows GOARCH=amd64 go build -o stashcli.exe .

# Linux (من Windows/macOS)
GOOS=linux GOARCH=amd64 go build -o stashcli .

# macOS (Apple Silicon)
GOOS=darwin GOARCH=arm64 go build -o stashcli .
```

---

## 2. الحصول على API ID/hash من تيليجرام

1. اذهب إلى <https://my.telegram.org> وسجّل الدخول برقم هاتفك.
2. افتح **API development tools** وأنشئ تطبيقًا (أي اسم يفي بالغرض).
3. انسخ قيمتي **api_id** و **api_hash** الظاهرتين — ستحتاجهما في
   الخطوة التالية.

---

## 3. الإعداد

انسخ ملف الإعدادات النموذجي واملأ بيانات API الخاصة بك:

```bash
cp settings.example.json settings.json
```

```jsonc
{
  "uploadchunksize": 471859200,   // 450 ميغابايت — اتركها كما هي، أو قلّلها إن كان اتصالك غير مستقر
  "api_id": 123456,               // من my.telegram.org
  "api_hash": "your_api_hash",    // من my.telegram.org
  "parral_download": 4,           // عدد الأجزاء التي تُنزَّل بالتوازي
  "parral_upload": 4,             // عدد الأجزاء التي تُرفع بالتوازي
  "cache_max_size_mb": 200,
  "cache_expire_days": 7
}
```

لست مضطرًا لضبط `uploadchunksize`/`parral_upload`/`parral_download` على
الإطلاق — إن تركتها فارغة (أو `0`)، فإن stashcli يستخدم افتراضيًا أجزاءً
بحجم 450 ميغابايت وتوازيًا رباعيًا من تلقاء نفسه.

> **اتصالك بطيء أو غير مستقر؟** اجعل قيمة `uploadchunksize` أصغر، مثل
> `16777216` (16 ميغابايت) أو `67108864` (64 ميغابايت). الأجزاء الأكبر
> أسرع على اتصال جيد، لكن انقطاع جزء واحد يعني إعادة إرسال ذلك الجزء
> بأكمله، لذا الأجزاء الأصغر أكثر أمانًا على اتصال غير موثوق.

بعد ذلك أنشئ قاعدة بيانات التخزين (الفارغة) وسجّل الدخول مرة واحدة:

```bash
./stashcli storage gen storage.json
./stashcli storage add
```

سيطلب منك أمر `storage add` رقم هاتفك (لتسجيل الدخول إلى تيليجرام) وواحد
أو أكثر من **معرّفات المحادثة (chat IDs)** — وهي الأماكن التي تُرسل إليها
أجزاء ملفاتك (مجموعة خاصة أو "الرسائل المحفوظة" كلاهما مناسب). يمكنك
الحصول على معرّف المحادثة من أي بوت تيليجرام يعرضه، أو بإعادة توجيه رسالة
من تلك المحادثة إلى `@userinfobot`/`@getidsbot`.

> يحتوي ملف `storage.json` على جلسة تيليجرام الفعلية الخاصة بك — تعامل
> معه كأنه كلمة مرور. لا تقم أبدًا برفعه إلى git أو مشاركته.

---

## 4. الاستخدام

### مباشرة من سطر الأوامر

```bash
# الرفع
./stashcli upload ./movie.mkv /movies/movie.mkv

# التنزيل
./stashcli download /movies/movie.mkv ./movie.mkv

# عرض محتويات مجلد
./stashcli ls /movies

# عرض البيانات الوصفية: الحجم/تاريخ الرفع لملف، أو عدد العناصر لمجلد
./stashcli info /movies/movie.mkv
./stashcli info /movies

# إنشاء مجلد، حذف ملف/مجلد
./stashcli mkdir /movies
./stashcli rm /movies/movie.mkv
```

### التركيب كقرص (WebDAV)

```bash
./stashcli webdav --port 8080 --user alice --pass secret
```

ثم اتصل من:
- **Windows:** File Explorer ← *This PC* ← *Map network drive* ← `http://127.0.0.1:8080`
- **macOS:** Finder ← *Go* ← *Connect to Server* ← `http://127.0.0.1:8080`
- **Linux:** أي عميل WebDAV، أو التركيب باستخدام `davfs2`/`rclone mount`

### التركيب كـ FTP

```bash
./stashcli ftp --port 21 --user alice --pass secret
```

اتصل بأي عميل FTP (FileZilla، WinSCP، أمر `ftp`، العميل المدمج في نظام
تشغيلك، إلخ).

### بث ملف مباشرة (دون الحاجة للتنزيل)

```bash
./stashcli stream --port 8081
```

ثم افتح `http://127.0.0.1:8081/movies/movie.mkv` في متصفح أو مشغّل وسائط
مثل VLC/mpv — يدعم التقديم/الترجيع دون تنزيل الملف بأكمله أولًا.

---

## 5. التشغيل كخدمة في الخلفية

### Linux (systemd)

أنشئ الملف `/etc/systemd/system/stashcli-webdav.service`:

```ini
[Unit]
Description=stashcli WebDAV server
After=network.target

[Service]
WorkingDirectory=/opt/stashcli
ExecStart=/opt/stashcli/stashcli webdav --port 8080 --user alice --pass secret
Restart=on-failure
User=youruser

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable --now stashcli-webdav
```

### Windows

شغّله من ملف `.bat`، أو استخدم [NSSM](https://nssm.cc/) لتثبيته كخدمة
Windows حقيقية تستمر بعد إعادة التشغيل.

---

## ملاحظات أمنية

- يتم تشفير كل جزء (AES-256-GCM) بكلمة مرور عشوائية خاصة به قبل رفعه —
  تيليجرام لا يرى محتوى ملفك الحقيقي أبدًا.
- خوادم FTP/WebDAV/البث **لا تحتوي على تشفير نقل مدمج**. أبقِها على
  `127.0.0.1` واستخدم نفق SSH، أو ضع reverse proxy بشهادة TLS أمامها إذا
  احتجت وصولًا عن بعد. اضبط دائمًا `--user`/`--pass` إن ربطتها بغير
  `127.0.0.1`.
- يحتوي ملف `storage.json` على سلسلة جلسة تيليجرام الحيّة — أي شخص يملك
  هذا الملف يملك وصولًا كاملًا لحسابك. احتفظ به في مكان خاص، ولا ترفعه
  أبدًا إلى git.

## خلف شبكة مُراقَبة أو مُقيَّدة؟

يمكن لـ stashcli توجيه اتصال تيليجرام عبر بروكسي SOCKS5 أو MTProto —
راجع حقل `proxy` الموثّق في `configs.go`، أو ضع
`"proxy": {"type": "system"}` في `settings.json` للكشف التلقائي عن
بروكسي SOCKS المُعدّ في نظام التشغيل (مدعوم على Windows وLinux/GNOME/KDE).