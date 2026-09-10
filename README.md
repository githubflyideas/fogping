A smokeping-like network tool
-----One binary file-----
Just scp and run

fogping is the SQLite build of the smoke-graph probe: same oscilloscope UI, same
zero-config target lists, but rounds land in an embedded SQLite file with
in-process rollup and retention. Its sibling [pingping](https://github.com/githubflyideas/pingping)
keeps the plain-JSONL storage.

```bash
mkdir -p /home/fogping && cd /home/fogping

wget https://github.com/githubflyideas/fogping/releases/download/v1.0.0/fogping-v1.0.0-linux-amd64.tar.gz
tar -zxvf fogping-v1.0.0-linux-amd64.tar.gz
./fogping user=admin passwd=admin
```

Or build it yourself (cgo required):

```bash
git clone https://github.com/githubflyideas/fogping.git && cd fogping
CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -o fogping .
```
Open http://localhost:8518 and watch your first puff of network smoke

Storage is SQLite (`data/fogping.db`); history is kept for 40 days by default
(`--days=300` for longer). Rollup and pruning run in-process — there is nothing
to cron.

Build note: SQLite goes through `mattn/go-sqlite3`, so cgo is required and
linux/amd64 is the only released target. The published tarball is statically
linked — it does not depend on the build host's glibc.

-----------------------------------------------------------
Targets
-------
Targets live in the SQLite database. The web UI is read-only by default; start with
`--edit` to add, edit and delete targets from the browser, then restart without it:

```bash
./fogping --edit user=admin passwd=admin     # editable (needs a login, or --localhost)
./fogping user=admin passwd=admin            # everyday: read-only
```

`targets/` is an import inbox for scripts and first-time setup. Drop a list there and
it is imported within a few seconds, then archived as `*.imported`:
```
 echo "1.2.3.4 myhost pace=fast"    >> targets/ping.list
 echo "10.0.0.5:443 ads-api"        >> targets/tcp.list
```
Import upserts by name (an existing target of the same name takes the new settings).
A file with a bad line is imported not at all and parked as `*.rejected`. Upgrading
from an older build: your existing `ping.list`/`tcp.list` are imported on first
start, and history carries over because it is keyed by target name.

Deleting a target stops probing but keeps its history until retention ages it out;
re-adding the same name picks the history back up. Renaming keeps history.

Run it as a service
```ini
# /etc/systemd/system/fogping.service
[Unit]
Description=fogping link-quality probe
After=network-online.target

[Service]
WorkingDirectory=/opt/fogping
ExecStart=/opt/fogping/fogping user=admin passwd=admin
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
```
```bash
sysctl -w net.ipv4.ping_group_range="0 2147483647"   # unprivileged ICMP, no root needed
systemctl enable --now fogping
```

🌐 [English](#english) · [中文](#中文) · [Español](#español) · [Français](#français) · [Português](#português) · [Deutsch](#deutsch) · [Русский](#русский) · [日本語](#日本語) · [한국어](#한국어) · [Bahasa Indonesia](#bahasa-indonesia) · [Tiếng Việt](#tiếng-việt) · [العربية](#العربية) · [हिन्दी](#हिन्दी) · [বাংলা](#বাংলা) · [اردو](#اردو) · [Türkçe](#türkçe) · [ไทย](#ไทย)
![window: a 40-minute congestion event — smoke spreads, bursts marked ◆](docs/hero15.png)
![window: a 40-minute congestion event — smoke spreads, bursts marked ◆](docs/hero17.png)
![window: a 40-minute congestion event — smoke spreads, bursts marked ◆](docs/hero16.png)


## English

FogPing is a lightweight network latency and link quality visualization tool.

It may not be as powerful or feature-rich as Smokeping, but it's ridiculously lightweight.

Single binary
No Docker
No make. Just scp and run.
Embedded SQLite — one file, no server, no setup
Plain text configuration. Edit targets with your favorite editor—or even a single echo command.

Download it, extract it, run ./fogping, then go grab a coffee.

When you're back, open http://localhost:8518 and watch your first puff of network smoke.

## 中文

FogPing 是一款轻量级的网络链路质量绘图工具。它或许没有 Smokeping 那么强大、成熟，但它足够轻巧。
单一可执行文件（Single Binary），下载即可使用，无需 Docker、无需 Root 权限、无需外部数据库（内置 SQLite，单文件落盘）。使用纯文本配置文件，可用任何文本编辑器修改监控目标，甚至一行 echo 指令即可完成。
下载、解压、运行 ./fogping，然后去泡杯咖啡吧。回来打开 http://localhost:8518 看看你的第一缕网络烟雾！

-------------------------------------------------------------------------------------------------
FogPing 是一款輕量級的網路鏈路品質繪圖工具。
它或許沒有 Smokeping 那麼強大、成熟，但它足夠輕巧。

單一執行檔（Single Binary），下載即可使用
無需 Docker
無需 Root 權限
內建 SQLite，單一檔案落盤
使用純文字設定檔，可用任何文字編輯器修改監控目標，甚至一行 echo 指令即可完成。
下載、解壓、執行 ./fogping，然後去泡杯咖啡吧。回來打開 http://localhost:8518 看看你的第一縷網路煙霧！


## Español

FogPing es una herramienta ligera para visualizar la calidad de enlaces de red.

Puede que no sea tan potente ni tan completa como Smokeping, pero es extremadamente ligera.

Un único ejecutable
Sin Docker
Sin make. Solo copia con scp y ejecútalo.
SQLite embebido: un solo archivo, sin servidor
Configuración en texto plano. Puedes editar los objetivos con cualquier editor, o incluso con un simple comando echo.

Descárgalo, descomprímelo y ejecuta ./fogping. Luego ve a prepararte un café.

Cuando vuelvas, abre http://localhost:8518 y disfruta de tu primera nube de humo de red.

## Français

FogPing est un outil léger de visualisation de la qualité des liaisons réseau.

Il n'est peut-être pas aussi puissant ni aussi complet que Smokeping, mais il est incroyablement léger.

Un seul exécutable
Aucun Docker
Pas de make. Un simple scp, puis exécutez-le.
SQLite embarqué : un seul fichier, aucun serveur
Configuration en texte brut. Modifiez les cibles avec votre éditeur préféré, ou même avec une simple commande echo.

Téléchargez-le, décompressez-le et lancez ./fogping.

Allez ensuite vous préparer un café.

À votre retour, ouvrez http://localhost:8518 et admirez votre premier nuage de fumée réseau.

## Português

FogPing é uma ferramenta leve para visualizar a qualidade das ligações de rede.

Talvez não seja tão poderoso quanto o Smokeping, mas é extremamente leve.

Binário único
Sem Docker
Sem make. Basta copiar com scp e executar.
SQLite embutido: um único ficheiro, sem servidor
Configuração em texto simples. Edite os alvos com qualquer editor ou até mesmo com um único comando echo.

Baixe, extraia e execute ./fogping.

Depois vá tomar um café.

Quando voltar, abra http://localhost:8518 e veja a sua primeira fumaça da rede.

## Deutsch

FogPing ist ein leichtgewichtiges Werkzeug zur Visualisierung der Netzwerkqualität.

Es ist vielleicht nicht so leistungsfähig wie Smokeping, dafür aber extrem schlank.

Eine einzige ausführbare Datei
Kein Docker
Kein make. Einfach per scp kopieren und starten.
Eingebettetes SQLite — eine Datei, kein Server
Konfiguration als Textdatei. Ziele lassen sich mit jedem Editor oder sogar mit einem einzigen echo-Befehl bearbeiten.

Herunterladen, entpacken und ./fogping starten.

Dann gönn dir einen Kaffee.

Wenn du zurückkommst, öffne http://localhost:8518 und sieh dir deine erste Netzwerk-Rauchwolke an.

## Русский

FogPing — лёгкий инструмент для визуализации качества сетевых соединений.

Возможно, он не такой мощный, как Smokeping, но зато невероятно лёгкий.

Один исполняемый файл
Без Docker
Без make. Просто скопируйте через scp и запустите.
Встроенный SQLite — один файл, без сервера
Текстовый файл конфигурации. Цели можно редактировать любым редактором или даже одной командой echo.

Скачайте, распакуйте и запустите ./fogping.

А затем сходите выпить кофе.

Вернувшись, откройте http://localhost:8518 и посмотрите на своё первое «сетевое облако дыма».


## 日本語

FogPing は、軽量なネットワーク品質可視化ツールです。

Smokeping ほど高機能ではありませんが、その代わり驚くほど軽量です。

単一バイナリ
Docker 不要
make 不要。scp して実行するだけ。
SQLite 内蔵。ファイル 1 つ、サーバも設定も不要
設定ファイルはプレーンテキスト。お好みのエディタで編集でき、echo 一行でも監視対象を追加できます。

ダウンロードして展開し、./fogping を実行したら、コーヒーでも淹れましょう。

戻って http://localhost:8518 を開けば、最初のネットワークスモークグラフが待っています。

## 한국어

FogPing은 가벼운 네트워크 품질 시각화 도구입니다.

Smokeping만큼 강력하지는 않지만, 놀라울 정도로 가볍습니다.

단일 실행 파일
Docker 불필요
make 불필요. scp로 복사한 뒤 바로 실행.
내장 SQLite — 파일 하나, 서버 불필요
설정은 일반 텍스트 파일입니다. 원하는 편집기로 수정하거나 echo 한 줄만으로도 모니터링 대상을 추가할 수 있습니다.

다운로드하고 압축을 푼 뒤 ./fogping을 실행하세요.

그리고 커피 한 잔 마시고 돌아오세요.

돌아와 http://localhost:8518 를 열면 첫 번째 네트워크 스모크 그래프를 볼 수 있습니다.


## Bahasa Indonesia

FogPing adalah alat ringan untuk memvisualisasikan kualitas koneksi jaringan.

Mungkin tidak sekuat Smokeping, tetapi sangat ringan.

Satu berkas biner
Tanpa Docker
Tanpa make. Cukup salin dengan scp lalu jalankan.
SQLite tertanam — satu berkas, tanpa server
Konfigurasi berbentuk teks biasa. Edit target dengan editor favorit Anda, atau bahkan cukup dengan satu perintah echo.

Unduh, ekstrak, lalu jalankan ./fogping.

Kemudian nikmati secangkir kopi.

Saat kembali, buka http://localhost:8518 dan lihat asap pertama jaringan Anda.

## Tiếng Việt

FogPing là công cụ nhẹ để trực quan hóa chất lượng kết nối mạng.

Có thể nó không mạnh bằng Smokeping, nhưng cực kỳ gọn nhẹ.

Một tệp thực thi duy nhất
Không cần Docker
Không cần make. Chỉ cần scp rồi chạy.
SQLite nhúng — một tệp duy nhất, không cần máy chủ
Cấu hình bằng tệp văn bản thuần túy. Bạn có thể chỉnh sửa bằng bất kỳ trình soạn thảo nào, hoặc chỉ với một lệnh echo.

Tải về, giải nén và chạy ./fogping.

Sau đó hãy đi pha một tách cà phê.

Khi quay lại, mở http://localhost:8518 để xem làn khói mạng đầu tiên của bạn.

## العربية

FogPing أداة خفيفة لعرض جودة اتصالات الشبكة.

قد لا تكون بنفس قوة Smokeping، لكنها خفيفة للغاية.

ملف تنفيذي واحد
لا حاجة إلى Docker
لا حاجة إلى make، فقط انسخه باستخدام scp ثم شغّله.
قاعدة بيانات SQLite مدمجة — ملف واحد بلا خادم
إعدادات بنص عادي، ويمكن تعديل أهداف المراقبة بأي محرر نصوص، أو حتى بأمر echo واحد.

نزّل البرنامج، فك الضغط، ثم شغّل ./fogping.

بعدها اذهب لتحضير فنجان من القهوة.

وعند عودتك، افتح http://localhost:8518 وشاهد أول مخطط دخان للشبكة.

## हिन्दी

FogPing एक हल्का नेटवर्क लिंक गुणवत्ता विज़ुअलाइज़ेशन टूल है।

यह Smokeping जितना शक्तिशाली नहीं हो सकता, लेकिन बेहद हल्का है।

एकल बाइनरी
Docker की आवश्यकता नहीं
make की आवश्यकता नहीं। बस scp करें और चलाएँ।
अंतर्निहित SQLite — एक फ़ाइल, कोई सर्वर नहीं
साधारण टेक्स्ट कॉन्फ़िगरेशन। अपनी पसंद के किसी भी संपादक से लक्ष्य बदलें, या केवल एक echo कमांड से।

डाउनलोड करें, अनज़िप करें और ./fogping चलाएँ।

फिर एक कप कॉफ़ी बना लीजिए।

वापस आकर http://localhost:8518 खोलें और अपना पहला नेटवर्क स्मोक ग्राफ़ देखें।

## বাংলা

FogPing একটি হালকা নেটওয়ার্ক সংযোগের মান প্রদর্শনের টুল।

এটি Smokeping-এর মতো শক্তিশালী নাও হতে পারে, তবে অত্যন্ত হালকা।

একটি মাত্র বাইনারি
Docker প্রয়োজন নেই
make প্রয়োজন নেই। শুধু scp করে চালান।
অন্তর্নির্মিত SQLite — একটিমাত্র ফাইল, কোনো সার্ভার নয়
সাধারণ টেক্সট কনফিগারেশন। যেকোনো টেক্সট এডিটর, এমনকি একটি echo কমান্ড দিয়েও মনিটরিং লক্ষ্য পরিবর্তন করা যায়।

ডাউনলোড করুন, আনজিপ করুন এবং ./fogping চালান।

তারপর এক কাপ কফি বানিয়ে আসুন।

ফিরে এসে http://localhost:8518 খুলুন এবং আপনার প্রথম নেটওয়ার্ক স্মোক গ্রাফ দেখুন।

## اردو

FogPing نیٹ ورک لنک کے معیار کو دکھانے والا ایک ہلکا پھلکا ٹول ہے۔

یہ شاید Smokeping جتنا طاقتور نہ ہو، لیکن انتہائی ہلکا ہے۔

ایک واحد بائنری
Docker کی ضرورت نہیں
make کی ضرورت نہیں۔ صرف scp کریں اور چلائیں۔
بلٹ اِن SQLite — ایک فائل، کوئی سرور نہیں
سادہ ٹیکسٹ کنفیگریشن۔ کسی بھی ایڈیٹر یا صرف ایک echo کمانڈ سے مانیٹرنگ اہداف تبدیل کیے جا سکتے ہیں۔

ڈاؤن لوڈ کریں، ان زپ کریں اور ./fogping چلائیں۔

پھر ایک کپ کافی بنا لیں۔

واپس آ کر http://localhost:8518 کھولیں اور اپنا پہلا نیٹ ورک اسموک گراف دیکھیں۔

## Türkçe

FogPing, ağ bağlantısı kalitesini görselleştiren hafif bir araçtır.

Smokeping kadar güçlü olmayabilir, ancak son derece hafiftir.

Tek çalıştırılabilir dosya
Docker gerekmez
make gerekmez. scp ile kopyalayın ve çalıştırın.
Gömülü SQLite — tek dosya, sunucu yok
Düz metin yapılandırması. Hedefleri istediğiniz düzenleyiciyle, hatta tek bir echo komutuyla bile değiştirebilirsiniz.

İndirin, arşivi açın ve ./fogping çalıştırın.

Sonra gidip bir kahve hazırlayın.

Geri döndüğünüzde http://localhost:8518 adresini açın ve ilk ağ duman grafiğinizi görün.

## ไทย

FogPing เป็นเครื่องมือขนาดเล็กสำหรับแสดงภาพคุณภาพของลิงก์เครือข่าย

อาจจะไม่ได้ทรงพลังหรือมีฟีเจอร์ครบถ้วนเทียบเท่า Smokeping แต่มีจุดเด่นคือความเบาและใช้งานง่ายอย่างมาก

ไฟล์ไบนารีเดียว (Single Binary)
ไม่ต้องใช้ Docker
ไม่ต้องใช้ make เพียง scp ไฟล์แล้วใช้งานได้ทันที
ใช้ SQLite ในตัว — ไฟล์เดียว ไม่ต้องติดตั้งเซิร์ฟเวอร์
ใช้ไฟล์กำหนดค่าแบบข้อความธรรมดา สามารถแก้ไขเป้าหมายการตรวจสอบด้วยโปรแกรมแก้ไขข้อความใดก็ได้ หรือแม้แต่ใช้คำสั่ง echo เพียงบรรทัดเดียว

ดาวน์โหลด แตกไฟล์ แล้วรัน ./fogping

จากนั้นไปชงกาแฟสักแก้ว

เมื่อกลับมา เปิด http://localhost:8518 แล้วดูควันเครือข่ายเส้นแรกของคุณได้เลย





-----------------------------------------------------------------
Friendly Links smokeping--- https://github.com/oetiker/SmokePing

- ⭐ Star 
- [GitHub Sponsors](https://github.com/sponsors/githubflyideas) 



## Design notes

[#design-notes](#design-notes)

fogping is a minimalist smokeping-like network oscilloscope with SQLite storage:
raw rounds are held hot for 2 days, then rolled up hourly and kept for 40 days
(`--days` to change). Burst detection (z-score) writes its verdict alongside each
round, so the ◆ marks on the chart come straight from the store. Built-in Web UI
with native auth — read-only unless started with `--edit` — no Nginx, no Caddy, no
external database. Targets are rows in the same SQLite file; there is no config file.

Storage is the only thing that separates it from its sibling
[pingping](https://github.com/githubflyideas/pingping), which writes plain JSONL
and has no cgo dependency at all. Pick pingping if you want a pure-Go static
binary and grep-able data files; pick fogping if you want indexed queries and
in-process retention.

## License

apache 2.0
