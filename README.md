# 🎥 TikTok Live Recorder - Go Straight / Suckless Edition

這是我為了實現 24/7 全天候監控 TikTok 直播，並將系統資源耗損降到最低，基於 **Unix / Suckless 哲學** 打造的極簡單檔守護進程（Daemon）。

**核心開發準則：**

不造多餘的輪子、不用肥大的框架、不寫無謂的硬碟 I/O，一切向 Linux Kernel 借力。

原本需要多個 Shell 腳本與外部 API 的複雜架構，如今全部優雅地收攏在單一個 `main.go` 中，一鍵背景化、一鍵清場、一滴不留。

---

# 🧠 一、核心原理與實作細節（How It Actually Works）

這裡記錄了這套系統最精華的底層運維與黑魔法技術細節，以免未來忘記當初為什麼這樣寫。

---

## 1. 零 I/O 狀態監控與狀態校正（Zero-I/O Introspection）

### 舊版問題

雷達腳本在背景休眠倒數時，面板若想知道剩餘秒數：

* 必須透過硬碟讀寫文字檔
* 或透過 `pgrep` + `ps -o etimes=`

向作業系統 Process Tree 內省計算時間差。

---

### 新版 Go 實作

全面改用 Go 記憶體管線控制。

整個雷達的步進倒數完全在記憶體中維護：

* Mutex 保護狀態
* 原子更新
* 零延遲讀取

當雷達檢測到開播時：

1. 1 秒內鎖定狀態
2. 強制切換 API 與網頁顯示
3. 顯示：

```text
🟢 已交接錄影
```

之後才進入錄影阻塞流程。

徹底修復舊版：

```text
🟢 已交接錄影
🟣 發送網路請求中
```

同時出現的狀態錯亂問題。

全程：

```text
0 Disk I/O
```

---

## 2. 即時 I/O 速度與磁碟追蹤（ProcFS & OS Introspection）

### 問題

如何在不引入：

* psutil
* 大型監控框架

的情況下取得：

* 剩餘空間
* 寫入速度
* 系統狀態

---

### 實作

直接向 Linux Kernel 借力。

#### 儲存空間

使用：

```bash
df -B1
```

解析 Byte 級資訊：

* Total
* Used
* Free

完全無快取。

---

#### 寫入速度

透過：

```go
os.Stat()
```

秒級輪詢錄影中的 `.ts` 檔案。

計算：

```text
目前大小 - 上秒大小
```

即可得到：

```text
KiB/s
MiB/s
```

即時寫入速率。

---

## 3. 底層效能壓榨（Performance Tuning）

為了長駐錄影不卡死伺服器。

---

### taskset

```bash
taskset -c 0
```

將：

* streamlink
* ffmpeg

綁定至 CPU Core 0。

減少：

* Context Switch
* CPU Cache Miss

---

### ionice

```bash
ionice -c 2 -n 0
```

最高優先級 Best-effort I/O。

確保：

* 不掉幀
* 不塞車
* 不延遲寫入

---

### jemalloc

```bash
LD_PRELOAD=/usr/lib/libjemalloc.so
```

取代 glibc malloc。

優勢：

* 降低碎片化
* 長期穩定
* 避免記憶體膨脹

---

## 4. 自動垃圾回收（Garbage Collection）

錄影開始時記錄：

```go
time.Now()
```

錄影結束時計算：

```text
Lifespan
```

若：

```text
存活 < 5 秒
```

或：

```text
檔案大小 = 0
```

立即：

```go
os.Remove()
```

刪除垃圾檔。

保持磁碟乾淨。

---

## 5. 網頁端免重整刷新（Vanilla AJAX Panel）

### 問題

想看：

* 錄影狀態
* 影片大小
* 寫入速度

但不想：

* React
* Vue
* Angular

---

### 實作

內建：

```go
http.ListenAndServe()
```

搭配：

```go
html/template
```

前端僅使用：

```javascript
setInterval()
```

每秒：

```javascript
fetch('/api/status')
```

更新：

```javascript
innerHTML
```

實現超輕量動態儀表板。

---

# 🏗️ 二、系統架構：單檔守護者（Single Binary Daemon）

完全遵循：

```text
Single Binary Philosophy
```

---

## 1. config.json

負責：

* 排程
* 時間窗
* Port
* TikTok URL

主程式保持 Immutable。

---

## 2. Self-Forking Daemon

執行：

```bash
./livetool start
```

若偵測前景執行：

* 自我 Fork
* 背景化
* 脫離終端

並將：

```text
stdout
stderr
```

導向：

```text
livetool.log
```

因此：

```bash
nohup xxx &
```

完全不需要。

---

## 3. Trident Kill 防線

執行：

```bash
./livetool stop
```

流程：

### 第一刀

通知 API：

```text
優雅停機
```

---

### 第二刀

強制清場：

```bash
pkill -9 streamlink
pkill -9 ffmpeg
```

---

### 第三刀

自殺：

```bash
pkill -9 livetool
```

徹底蒸發。

---

# ⚙️ 三、環境編譯（Build）

## Ubuntu / Debian

```bash
sudo apt update
sudo apt install golang ffmpeg -y
```

---

## Arch Linux

```bash
sudo pacman -Syu go ffmpeg
```

---

## 編譯

```bash
go build -o livetool main.go
```

---

# 📦 四、部署流程（Deployment）

## 1. 核心工具箱準備

🔥 最重要步驟

建立獨立 Streamlink 環境。

```bash
python3 -m venv venv
```

```bash
source venv/bin/activate
```

```bash
pip install --upgrade streamlink
```

```bash
deactivate
```

---

### 備註

主程式啟動時會自動：

```text
PATH 優先插入：
./venv/bin
```

確保使用最新版 Streamlink。

---

## 2. config.json

```json
{
  "target_url": "https://www.tiktok.com/@example_channel",
  "web_port": 8080,
  "probe_start": "13:30",
  "probe_end": "00:00",
  "probe_interval": 30,
  "probe_sleep_deep": 300
}
```

---

# 🚀 五、快速操作手冊（Usage Instructions）

## 🟩 啟動監控服務

```bash
./livetool start
```

立即背景化。

---

### 查看日誌

```bash
tail -f livetool.log
```

---

### 網頁面板

```text
http://SERVER_IP:8080
```

---

## 📊 終端機查看狀態

```bash
./livetool status
```

顯示：

* 錄影狀態
* 雷達狀態
* 剩餘倒數
* 影片大小
* 寫入速度
* 磁碟剩餘空間

---

## 🟥 優雅關閉

```bash
./livetool stop
```

執行：

1. 優雅停機
2. 存檔
3. 強制清場
4. 蒸發殘留進程

---

# 📋 六、CLI Cheat Sheet

| 指令                  | 功能     |
| ------------------- | ------ |
| `./livetool start`  | 啟動背景監控 |
| `./livetool status` | 查看即時狀態 |
| `./livetool stop`   | 停止並清場  |

---

# 📂 七、專案目錄架構

```text
~/go_straight/
├── livetool
├── config.json
├── livetool.log
├── venv/
└── downloads/
    └── 頻道名稱/
        ├── 20260604-133015.ts
        ├── 20260604-153021.ts
        └── ...
```

---

# 🎯 設計哲學總結

本專案的核心目標只有一句話：

> 能用 Linux Kernel 做的事，就不要自己做。

因此：

* 不依賴重量級框架
* 不建立多餘中介層
* 不寫垃圾狀態檔
* 不做無意義硬碟 I/O

整個系統由單一 Go Binary 控制：

```text
TikTok
   ↓
streamlink
   ↓
ffmpeg
   ↓
.ts
```

配合：

* Self-Forking Daemon
* ProcFS Introspection
* Zero-I/O State Engine
* Trident Kill Cleanup

實現真正可長期 24/7 運行的 TikTok Live Recorder。
