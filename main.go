package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config 設定結構體 (對應 config.json)
type Config struct {
	TargetURL      string `json:"target_url"`
	WebPort        int    `json:"web_port"`
	ProbeStart     string `json:"probe_start"`
	ProbeEnd       string `json:"probe_end"`
	ProbeInterval  int    `json:"probe_interval"`
	ProbeSleepDeep int    `json:"probe_sleep_deep"`
}

// GlobalState 共享核心狀態
type GlobalState struct {
	mu           sync.Mutex
	IsRecording  bool    `json:"is_recording"`
	ProbeStatus  string  `json:"probe_status"`
	LatestFile   string  `json:"latest_file"`
	LatestSize   int64   `json:"latest_size"`   // 純數字 (Bytes)
	LatestMTime  string  `json:"latest_mtime"`
	DiskTotal    uint64  `json:"disk_total"`    // 純數字 (Bytes)
	DiskAvail    uint64  `json:"disk_avail"`    // 純數字 (Bytes)
	DiskUsed     uint64  `json:"disk_used"`     // 純數字 (Bytes)
	CPULoad      string  `json:"cpu_load"`      // 實時 CPU 使用率百分比字串 (例如 "12.5%")
	RAMPercent   float64 `json:"ram_percent"`  // 實時記憶體使用率百分比 (0.0 ~ 100.0)
}

var (
	config       Config
	state        GlobalState
	saveDir      string
	prefix       string
	recordCtx    context.Context
	recordCancel context.CancelFunc

	// 用於實時 CPU 百分比微積分計算的背景變數
	lastTotalTime uint64
	lastIdleTime  uint64
)

func main() {
	// 1. 讀取與解析設定檔 config.json
	confFile, err := os.ReadFile("config.json")
	if err != nil {
		log.Fatalf("[ERROR] 無法讀取 config.json: %v", err)
	}
	if err := json.Unmarshal(confFile, &config); err != nil {
		log.Fatalf("[ERROR] 解析 config.json 失敗: %v", err)
	}

	// 解析 TikTok 頻道名稱
	parts := strings.Split(config.TargetURL, "@")
	if len(parts) < 2 {
		log.Fatalf("[ERROR] Target URL 格式錯誤，必須包含 @")
	}
	prefix = strings.Split(parts[1], "/")[0]

	// 【專案路徑】：完全在 go_straight 目錄下建立 downloads 資料夾
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	saveDir = filepath.Join(cwd, "downloads", prefix)
	_ = os.MkdirAll(saveDir, 0755)

	// 2. 檢查命令列控制參數 (CLI 功能)
	if len(os.Args) > 1 {
		action := os.Args[1]
		switch action {
		case "status":
			showCLIStatus()
			return
		case "stop", "shutdown":
			terminateService()
			return
		case "start":
			// 【內建背景核心實現】：如果不是背景環境，就自動把自己複製到背景跑
			if os.Getenv("LIVETOOL_DAEMON") != "1" {
				logFile, err := os.OpenFile("livetool.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
				if err != nil {
					log.Fatalf("[ERROR] 無法建立日誌檔案 livetool.log: %v", err)
				}

				// 準備在背景拉起一個一模一樣的自己，但注入一個環境變數 LIVETOOL_DAEMON=1
				cmd := exec.Command(os.Args[0], "start")
				cmd.Env = append(os.Environ(), "LIVETOOL_DAEMON=1")
				
				// 完美的 > livetool.log 2>&1 內建實現
				cmd.Stdout = logFile
				cmd.Stderr = logFile

				if err := cmd.Start(); err != nil {
					log.Fatalf("[ERROR] 自動切換至背景運行失敗: %v", err)
				}

				// 提示使用者後，主動結束當前前景進程，釋放終端機！
				fmt.Printf("\x1b[32m[LAUNCH] 🚀 go_straight 核心已成功射入背景運行！\x1b[0m\n")
				fmt.Printf("📂 日誌已自動導向 -> livetool.log (可使用 tail -f livetool.log 查看)\n")
				fmt.Printf("📊 網頁儀表板 -> http://localhost:%d\n", config.WebPort)
				return
			}
			// 如果環境變數是 1，代表已經成功在背景了，直接往下走執行主程式
		default:
			fmt.Printf("未知參數: %s\n可用指令:\n  ./livetool start    (自動在背景啟動主服務)\n  ./livetool status   (查看目前狀態儀表板)\n  ./livetool stop     (優雅關閉服務與背景小弟)\n", action)
			return
		}
	} else {
		fmt.Printf("提示: 請使用 ./livetool start 啟動服務。\n")
		return
	}

	// ==============================================================================
	// 以下皆為真正的背景主程序運行區 (日誌會自動寫入 livetool.log)
	// ==============================================================================

	log.Printf("[SYSTEM] go_straight 背景核心啟動成功。目標頻道: @%s", prefix)
	log.Printf("[SYSTEM] 錄影儲存路徑: %s", saveDir)

	// 自動載入本地 Python 虛擬環境
	if fi, err := os.Stat(filepath.Join(cwd, "venv", "bin")); err == nil && fi.IsDir() {
		os.Setenv("PATH", filepath.Join(cwd, "venv", "bin")+":"+os.Getenv("PATH"))
		log.Printf("[SYSTEM] 成功載入本地 Python 虛擬環境工具箱")
	}

	// 初始化磁碟資訊與系統狀態
	updateDiskStatus()
	updateSystemResource()
	state.ProbeStatus = "⚪ 系統初始化完成，雷達待命中..."

	// 啟動智慧哨兵雷達 (背景線程)
	go templateProbeRadar()

	// 註冊工業級 Web 路由
	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/api/status", handleAPIStatus)
	http.HandleFunc("/api/shutdown", handleAPIShutdown)

	addr := fmt.Sprintf(":%d", config.WebPort)
	log.Printf("=========================================================")
	log.Printf("🚀 go_straight 儀表板背景監聽中: http://localhost%s", addr)
	log.Printf("=========================================================")

	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("[ERROR] Web 伺服器啟動失敗: %v", err)
	}
}

// ==============================================================================
// CLI 終端機控制器邏輯 (與網頁端共享極簡核心)
// ==============================================================================

func showCLIStatus() {
	url := fmt.Sprintf("http://localhost:%d/api/status", config.WebPort)
	resp, err := http.Get(url)
	if err != nil {
		fmt.Println("\x1b[31m[OFFLINE] 核心服務未啟動！請先執行 ./livetool start 啟動服務。\x1b[0m")
		return
	}
	defer resp.Body.Close()

	var s struct {
		IsRecording bool    `json:"is_recording"`
		ProbeStatus string  `json:"probe_status"`
		LatestFile  string  `json:"latest_file"`
		LatestSize  int64   `json:"latest_size"`
		LatestMTime string  `json:"latest_mtime"`
		DiskTotal   uint64  `json:"disk_total"`
		DiskAvail   uint64  `json:"disk_avail"`
		DiskUsed    uint64  `json:"disk_used"`
		CPULoad     string  `json:"cpu_load"`
		RAMPercent  float64 `json:"ram_percent"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		fmt.Println("[ERROR] 解析伺服器回傳狀態失敗。")
		return
	}

	gb := float64(1024 * 1024 * 1024)
	pct := 0.0
	if s.DiskTotal > 0 {
		pct = (float64(s.DiskUsed) / float64(s.DiskTotal)) * 100
	}

	fmt.Println("\x1b[36m=========================================================\x1b[0m")
	fmt.Println("              📊 go_straight 核心監控儀表板")
	fmt.Println("\x1b[36m=========================================================\x1b[0m")
	if s.IsRecording {
		fmt.Println(" 🎬 錄影狀態: \x1b[31m● 錄影中 (RECORDING)\x1b[0m")
		fmt.Printf(" 📂 當前檔案: %s\n", s.LatestFile)
		fmt.Printf(" ⚖️ 檔案大小: %.2f MB\n", float64(s.LatestSize)/(1024*1024))
		fmt.Printf(" 🕒 更新時間: %s\n", s.LatestMTime)
	} else {
		fmt.Println(" 🎬 錄影狀態: \x1b[90m○ 未開播 (IDLE)\x1b[0m")
	}
	fmt.Printf(" 📡 雷達狀態: %s\n", s.ProbeStatus)
	fmt.Printf(" 💾 磁碟空間: 已用 %.2f GB / 總共 %.2f GB (可用 %.2f GB, 已使用 %.1f%%)\n", 
		float64(s.DiskUsed)/gb, float64(s.DiskTotal)/gb, float64(s.DiskAvail)/gb, pct)
	fmt.Printf(" ⚡ 系統效能: CPU 使用率: %s | RAM 使用率: %.1f%%\n", s.CPULoad, s.RAMPercent)
	fmt.Println("\x1b[36m=========================================================\x1b[0m")
}

// CLI 的 stop 命令（雙重保險，強力清場防線）
func terminateService() {
	url := fmt.Sprintf("http://localhost:%d/api/shutdown", config.WebPort)
	_, err := http.Get(url)
	if err != nil {
		fmt.Println("\x1b[33m[INFO] 核心服務未運行（或已卡死），啟動強制超度防線...\x1b[0m")
	} else {
		fmt.Println("\x1b[32m[SUCCESS] 已成功發送遠端關閉訊號，進程正優雅存檔退場...\x1b[0m")
	}

	// 終端機端也無情補刀，強行滅殺可能殘留的 streamlink 與 ffmpeg 小弟
	_ = exec.Command("pkill", "-9", "-f", "streamlink").Run()
	_ = exec.Command("pkill", "-9", "-f", "ffmpeg").Run()
	
	// 延遲一下，連同 livetool 的主進程也一起物理蒸發，杜絕任何幽靈殭屍
	time.Sleep(300 * time.Millisecond)
	_ = exec.Command("pkill", "-9", "-f", "livetool start").Run()
	
	fmt.Println("\x1b[32m[CLEAN] ✨ 背景所有殘留殭屍程序已徹底殺乾淨，一滴不留！\x1b[0m")
}

// ==============================================================================
// 智慧雷達與錄影核心 (狀態顯示修復優化版)
// ==============================================================================

func templateProbeRadar() {
	for {
		inWindow, timeToStart := checkTimeWindow(config.ProbeStart, config.ProbeEnd)

		state.mu.Lock()
		isRec := state.IsRecording
		state.mu.Unlock()

		// 如果已經在錄影中，雷達就徹底安靜，每 5 秒同步一次顯示文字即可
		if isRec {
			updateProbeStatus("🟢 已交接錄影 (哨兵常駐監聽中)")
			time.Sleep(5 * time.Second)
			continue
		}

		if !inWindow {
			totalSleepSec := int(timeToStart.Seconds())
			if timeToStart >= time.Duration(config.ProbeSleepDeep)*time.Second {
				totalSleepSec = config.ProbeSleepDeep
			}

			// 秒級步進倒數
			for i := totalSleepSec; i > 0; i-- {
				state.mu.Lock()
				if state.IsRecording {
					state.mu.Unlock()
					break
				}
				state.mu.Unlock()

				h := i / 3600
				m := (i % 3600) / 60
				s := i % 60
				updateProbeStatus(fmt.Sprintf("💤 非戰備時段，深度休眠中 (倒數 %02d:%02d:%02d 醒來檢測)", h, m, s))
				time.Sleep(1 * time.Second)
			}
			continue
		}

		updateProbeStatus("🟣 發送網路請求中 (檢測開播狀態...)")
		if checkLiveStatus() {
			log.Printf("[PROBE] ⚠️ 發現目標 @%s 正在直播！派遣錄影核心...", prefix)

			// 【Bug 核心修正點】：在進入錄影引擎前，搶先鎖定並同步刷新所有狀態與文字，防止時間差卡死
			state.mu.Lock()
			state.IsRecording = true
			state.ProbeStatus = "🟢 已交接錄影 (哨兵常駐監聽中)" 
			recordCtx, recordCancel = context.WithCancel(context.Background())
			state.mu.Unlock()

			// 這裡會阻塞（卡住）直到直播結束
			runRecordEngine(recordCtx)

			// 直播結束，安全釋放
			state.mu.Lock()
			state.IsRecording = false
			if recordCancel != nil {
				recordCancel()
			}
			state.mu.Unlock()

			time.Sleep(10 * time.Second)
		} else {
			waitTime := config.ProbeInterval + rand.Intn(21)
			for i := waitTime; i > 0; i-- {
				state.mu.Lock()
				if state.IsRecording {
					state.mu.Unlock()
					break
				}
				state.mu.Unlock()

				updateProbeStatus(fmt.Sprintf("🟡 刺探待命中 (倒數 %d 秒後發起偵測)", i))
				time.Sleep(1 * time.Second)
			}
		}
	}
}

func updateProbeStatus(status string) {
	state.mu.Lock()
	state.ProbeStatus = status
	state.mu.Unlock()
}

func checkTimeWindow(startStr, endStr string) (bool, time.Duration) {
	if startStr == endStr {
		return true, 0
	}
	now := time.Now()
	todayStr := now.Format("2006-01-02")

	start, _ := time.ParseInLocation("2006-01-02 15:04", todayStr+" "+startStr, time.Local)
	end, _ := time.ParseInLocation("2006-01-02 15:04", todayStr+" "+endStr, time.Local)

	if end.Before(start) {
		if now.Before(end) {
			start = start.AddDate(0, 0, -1)
		} else {
			end = end.AddDate(0, 0, 1)
		}
	}

	if now.After(start) && now.Before(end) {
		return true, 0
	}

	var nextStart time.Time
	if now.Before(start) {
		nextStart = start
	} else {
		nextStart = start.AddDate(0, 0, 1)
	}
	return false, nextStart.Sub(now)
}

func checkLiveStatus() bool {
	cmd := exec.Command("streamlink", "--json", config.TargetURL)
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), "best")
}

func runRecordEngine(ctx context.Context) {
	tsFile := filepath.Join(saveDir, time.Now().Format("20060102-150405")+".ts")
	log.Printf("[CORE] 錄影開始: %s", filepath.Base(tsFile))

	startTime := time.Now()

	streamlinkCmd := exec.CommandContext(ctx, "taskset", "-c", "0", "ionice", "-c", "2", "-n", "0",
		"streamlink", config.TargetURL, "hd,ld,best",
		"--ringbuffer-size", "512M",
		"--stream-segment-threads", "1",
		"--stream-timeout", "60",
		"--http-header", "Referer=https://www.tiktok.com/",
		"--http-header", "Origin=https://www.tiktok.com",
		"--http-header", "User-Agent=Mozilla/5.0 (X11; Linux x86_64; rv:126.0) Gecko/20100101 Firefox/126.0",
		"-O",
	)

	ffmpegCmd := exec.CommandContext(ctx, "taskset", "-c", "0", "ionice", "-c", "2", "-n", "0",
		"ffmpeg", "-y", "-i", "pipe:0", "-c", "copy", "-f", "mpegts", tsFile,
	)

	streamlinkCmd.Env = append(os.Environ(), "LD_PRELOAD=/usr/lib/libjemalloc.so")
	ffmpegCmd.Env = append(os.Environ(), "LD_PRELOAD=/usr/lib/libjemalloc.so")

	pipe, err := streamlinkCmd.StdoutPipe()
	if err != nil {
		log.Printf("[CORE] 建立 Pipe 失敗: %v", err)
		return
	}
	ffmpegCmd.Stdin = pipe

	if err := streamlinkCmd.Start(); err != nil {
		log.Printf("[CORE] Streamlink 啟動失敗: %v", err)
		return
	}
	if err := ffmpegCmd.Start(); err != nil {
		log.Printf("[CORE] FFmpeg 啟慢失敗: %v", err)
		return
	}

	// 實時秒級刷新寫入檔案的大小
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				fi, err := os.Stat(tsFile)
				if err == nil {
					state.mu.Lock()
					state.LatestFile = filepath.Base(tsFile)
					state.LatestMTime = fi.ModTime().Format("2006-01-02 15:04:05")
					state.LatestSize = fi.Size()
					state.mu.Unlock()
				}
				time.Sleep(1 * time.Second)
			}
		}
	}()

	_ = streamlinkCmd.Wait()
	_ = ffmpegCmd.Wait()

	lifespan := time.Since(startTime)
	fi, err := os.Stat(tsFile)

	if err != nil || fi.Size() == 0 || lifespan < 5*time.Second {
		_ = os.Remove(tsFile)
		log.Printf("[CORE] 移除垃圾片段 (%d 秒)。直播已結束。", int(lifespan.Seconds()))
	} else {
		log.Printf("[CORE] 錄影儲存成功 (%d 秒)。直播已結束。", int(lifespan.Seconds()))
	}
}

// ==============================================================================
// 網頁與 API 伺服器 (核心極簡原生 Linux 資源讀取)
// ==============================================================================

func updateDiskStatus() {
	cmd := exec.Command("df", "-B1", saveDir)
	output, err := cmd.Output()
	if err != nil {
		return
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) < 2 {
		return
	}

	fields := strings.Fields(lines[1])
	if len(fields) < 4 {
		return
	}

	total, _ := strconv.ParseUint(fields[1], 10, 64)
	used, _ := strconv.ParseUint(fields[2], 10, 64)
	avail, _ := strconv.ParseUint(fields[3], 10, 64)

	state.mu.Lock()
	state.DiskTotal = total
	state.DiskUsed = used
	state.DiskAvail = avail
	state.mu.Unlock()
}

// Suckless 原生實時解析 Linux 效能資源 (零外部依賴庫)
func updateSystemResource() {
	// 1. 讀取精準實時 CPU 百分比 (/proc/stat 微積分差值計算)
	statData, err := os.ReadFile("/proc/stat")
	if err == nil {
		lines := strings.Split(string(statData), "\n")
		if len(lines) > 0 && strings.HasPrefix(lines[0], "cpu ") {
			fields := strings.Fields(lines[0])
			if len(fields) >= 5 {
				var user, nice, system, idle, iowait, irq, softirq uint64
				user, _ = strconv.ParseUint(fields[1], 10, 64)
				nice, _ = strconv.ParseUint(fields[2], 10, 64)
				system, _ = strconv.ParseUint(fields[3], 10, 64)
				idle, _ = strconv.ParseUint(fields[4], 10, 64)
				if len(fields) > 5 { iowait, _ = strconv.ParseUint(fields[5], 10, 64) }
				if len(fields) > 6 { irq, _ = strconv.ParseUint(fields[6], 10, 64) }
				if len(fields) > 7 { softirq, _ = strconv.ParseUint(fields[7], 10, 64) }

				currentIdle := idle + iowait
				currentNonIdle := user + nice + system + irq + softirq
				currentTotal := currentIdle + currentNonIdle

				state.mu.Lock()
				// 排除第一次冷啟動，從第二次採樣起計算精確差值百分比
				if lastTotalTime > 0 && currentTotal > lastTotalTime {
					totalDiff := currentTotal - lastTotalTime
					idleDiff := currentIdle - lastIdleTime
					cpuPercent := (float64(totalDiff - idleDiff) / float64(totalDiff)) * 100
					state.CPULoad = fmt.Sprintf("%.1f%%", cpuPercent)
				} else {
					state.CPULoad = "計算中..."
				}
				state.mu.Unlock()

				lastTotalTime = currentTotal
				lastIdleTime = currentIdle
			}
		}
	} else {
		state.mu.Lock()
		state.CPULoad = "N/A"
		state.mu.Unlock()
	}

	// 2. 讀取精準實時記憶體 使用率 (/proc/meminfo)
	memData, err := os.ReadFile("/proc/meminfo")
	if err == nil {
		var memTotal, memAvail float64
		lines := strings.Split(string(memData), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "MemTotal:") {
				fields := strings.Fields(line)
				if len(fields) > 1 {
					memTotal, _ = strconv.ParseFloat(fields[1], 64)
				}
			}
			if strings.HasPrefix(line, "MemAvailable:") {
				fields := strings.Fields(line)
				if len(fields) > 1 {
					memAvail, _ = strconv.ParseFloat(fields[1], 64)
				}
				break // 關鍵資料拿齊了就直接中斷，不浪費核心時脈
			}
		}
		if memTotal > 0 {
			state.mu.Lock()
			state.RAMPercent = ((memTotal - memAvail) / memTotal) * 100
			state.mu.Unlock()
		}
	}
}

func handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	updateDiskStatus() 
	updateSystemResource() // 每次 API 被輪詢時同步毫秒級刷新

	state.mu.Lock()
	defer state.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(state)
}

// 後端 API 關閉邏輯（新增連坐法物理超度）
func handleAPIShutdown(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"shutting_down"}`))
	log.Println("[SYSTEM] 收到遠端停止訊號，進程正優雅關閉中...")

	// 1. 先觸發 Context 取消，讓 Go 內部的錄影引擎優雅結尾
	state.mu.Lock()
	if recordCancel != nil {
		recordCancel()
	}
	state.mu.Unlock()

	// 2. 【核心大撲殺】使用 Linux 連坐法！強制清空背景可能殘留的 streamlink 與 ffmpeg 錄影小弟
	_ = exec.Command("pkill", "-f", "streamlink").Run()
	_ = exec.Command("pkill", "-f", "ffmpeg").Run()
	log.Println("[SYSTEM] 背景 streamlink 與 ffmpeg 殘留小弟已全數清空！")

	// 3. 延遲 1 秒退場，確保 JSON 回傳完畢
	go func() {
		time.Sleep(1 * time.Second)
		os.Exit(0)
	}()
}

type FileRow struct {
	Name      string
	SizeBytes int64
	MTime     string
	IsGrowing bool
}

type PageData struct {
	Prefix     string
	ProbeStart string
	ProbeEnd   string
	Files      []FileRow
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/download/") {
		fileName := strings.TrimPrefix(r.URL.Path, "/download/")
		http.ServeFile(w, r, filepath.Join(saveDir, fileName))
		return
	}

	files, _ := os.ReadDir(saveDir)
	var rows []FileRow

	state.mu.Lock()
	activeFile := ""
	if state.IsRecording {
		activeFile = state.LatestFile
	}
	state.mu.Unlock()

	for i := len(files) - 1; i >= 0; i-- {
		f := files[i]
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".ts") {
			continue
		}
		info, _ := f.Info()
		isGrowing := (f.Name() == activeFile)

		rows = append(rows, FileRow{
			Name:      f.Name(),
			SizeBytes: info.Size(),
			MTime:     info.ModTime().Format("2006-01-02 15:04:05"),
			IsGrowing: isGrowing,
		})
	}

	tmpl, err := template.New("index").Parse(htmlTemplate)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := PageData{
		Prefix:     prefix,
		ProbeStart: config.ProbeStart,
		ProbeEnd:   config.ProbeEnd,
		Files:      rows,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = tmpl.Execute(w, data)
}

// ==============================================================================
// 完美優雅的 Cyberpunk 科技風網頁模板 (完全去除舊名)
// ==============================================================================
const htmlTemplate = `<!DOCTYPE html>
<html lang="zh-TW">
<head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>go_straight Recorder Panel</title>
    <style>
        :root {
            --bg-main: #0a0a0c;
            --bg-card: #141419;
            --border-color: #24242e;
            --text-main: #e3e3ed;
            --text-muted: #84849a;
            --accent-blue: #0070f3;
            --accent-green: #10b981;
            --accent-red: #ef4444;
            --accent-orange: #f59e0b;
        }
        body {
            background-color: var(--bg-main); color: var(--text-main);
            font-family: -apple-system, BlinkMacSystemFont, sans-serif;
            margin: 0; padding: 30px; display: flex; justify-content: center;
        }
        .container { width: 100%; max-width: 1000px; }
        header { display: flex; justify-content: space-between; align-items: center; margin-bottom: 25px; }
        h2 { margin: 0; font-size: 24px; font-weight: 700; letter-spacing: -0.5px; }
        .badge { font-size: 13px; padding: 5px 12px; border-radius: 6px; font-weight: 600; display: inline-flex; align-items: center; gap: 6px; }
        .badge-offline { background: #22222a; color: var(--text-muted); border: 1px solid var(--border-color); }
        .badge-live { background: rgba(239, 68, 68, 0.15); color: var(--accent-red); border: 1px solid var(--accent-red); animation: pulse 2s infinite; }
        @keyframes pulse { 0%, 100% { opacity: 1; } 50% { opacity: 0.5; } }
        
        .monitor-card { background: var(--bg-card); border: 1px solid var(--border-color); border-radius: 12px; padding: 20px; margin-bottom: 30px; box-shadow: 0 4px 20px rgba(0,0,0,0.4); }
        .meta-row { display: flex; justify-content: space-between; align-items: center; margin-bottom: 12px; font-size: 14px; }
        .meta-label { color: var(--text-muted); display: flex; align-items: center; gap: 8px; }
        .meta-value { font-weight: 600; font-family: monospace; font-size: 15px; }
        
        .progress-bg { width: 100%; height: 8px; background: #0d0d11; border-radius: 4px; overflow: hidden; margin-bottom: 18px; border: 1px solid #1c1c24; }
        .progress-fill { height: 100%; width: 0%; background: var(--accent-blue); border-radius: 4px; transition: width 0.4s ease, background-color 0.3s; }
        .divider { height: 1px; background: var(--border-color); margin: 15px 0; }
        
        table { width: 100%; border-collapse: separate; border-spacing: 0; margin-top: 10px; background: var(--bg-card); border: 1px solid var(--border-color); border-radius: 12px; overflow: hidden; }
        th { background: #171721; color: var(--text-muted); font-size: 13px; font-weight: 600; padding: 14px 18px; text-align: left; border-bottom: 1px solid var(--border-color); }
        td { padding: 14px 18px; font-size: 14px; border-bottom: 1px solid var(--border-color); color: var(--text-main); }
        tr:last-child td { border-bottom: none; }
        tr:hover td { background: #181822; }
        
        .file-link { color: #58a6ff; text-decoration: none; font-weight: 600; }
        .file-link:hover { color: #8ab4f8; text-decoration: underline; }
        .row-growing { background: rgba(16, 185, 129, 0.02) !important; }
        .row-growing td { color: var(--accent-green) !important; }
        .row-growing .file-link { color: var(--accent-green) !important; }
        .pulse-dot { width: 8px; height: 8px; background: var(--accent-green); border-radius: 50%; display: inline-block; animation: pulse 1.2s infinite; }
    </style>
</head>
<body>
    <div class="container">
        <header>
            <h2>🎥 @{{.Prefix}} 直播自動錄影庫</h2>
            <span id="liveBadge" class="badge badge-offline">⚪ 未開播</span>
        </header>

        <div class="monitor-card">
            <div class="meta-row">
                <span class="meta-label">💾 系統儲存空間</span>
                <span id="diskText" class="meta-value">讀取中...</span>
            </div>
            <div class="progress-bg">
                <div id="diskBarFill" class="progress-fill"></div>
            </div>

            <div class="meta-row">
                <span class="meta-label">⚡ 系統實時負載 <span id="cpuText" style="color:var(--text-main); font-family:monospace; margin-left:5px;">(CPU: 讀取中...)</span></span>
                <span id="ramText" class="meta-value">RAM: 讀取中...</span>
            </div>
            <div class="progress-bg">
                <div id="ramBarFill" class="progress-fill" style="background-color: var(--accent-green);"></div>
            </div>

            <div class="divider"></div>
            <div class="meta-row" style="margin-bottom:0; padding-top:4px;">
                <span class="meta-label">📡 背景雷達狀態 <span style="font-size:12px; color:#444;">(排程窗: {{.ProbeStart}} ~ {{.ProbeEnd}})</span></span>
                <span id="probeText" class="meta-value" style="color: var(--accent-orange);">連線中...</span>
            </div>
        </div>

        <h3 style="font-size:16px; color:var(--text-muted); margin-bottom:12px; font-weight:600;">📂 已錄製歷史片段</h3>
        <table id="vidTable">
            <thead>
                <tr>
                    <th>檔案名稱</th>
                    <th>容量大小</th>
                    <th>最後修改時間</th>
                </tr>
            </thead>
            <tbody>
                {{range .Files}}
                <tr id="row-{{.Name}}" class="{{if .IsGrowing}}row-growing{{end}}">
                    <td>
                        {{if .IsGrowing}}<span class="pulse-dot" style="margin-right:6px;"></span>{{end}}
                        <a class="file-link" href="/download/{{.Name}}" download>{{.Name}}</a>
                    </td>
                    <td class="file-size" data-bytes="{{.SizeBytes}}">讀取中...</td>
                    <td class="file-mtime">{{.MTime}}</td>
                </tr>
                {{else}}
                <tr>
                    <td colspan="3" style="text-align:center; color:var(--text-muted); padding:30px;">目前尚無任何錄影檔案。雷達正在暗中為您守候...</td>
                </tr>
                {{end}}
            </tbody>
        </table>
    </div>

    <script>
        function formatBytes(bytes) {
            if (bytes === 0) return "0.00 MB";
            var k = 1024;
            var sizes = ['Bytes', 'KB', 'MB', 'GB', 'TB'];
            var i = Math.floor(Math.log(bytes) / Math.log(k));
            if (i < 2) i = 2; 
            var val = bytes / Math.pow(k, i);
            var unit = sizes[i];
            if (unit === 'GB' || unit === 'TB') {
                return "<b>" + val.toFixed(2) + " " + unit + "</b>";
            }
            return val.toFixed(2) + " " + unit;
        }

        document.querySelectorAll('.file-size').forEach(function(td) {
            var b = parseInt(td.getAttribute('data-bytes'));
            if(!isNaN(b)) td.innerHTML = formatBytes(b);
        });

        setInterval(function() {
            fetch('/api/status')
                .then(function(r) { return r.json(); })
                .then(function(data) {
                    var badge = document.getElementById("liveBadge");
                    badge.className = data.is_recording ? "badge badge-live" : "badge badge-offline";
                    badge.innerHTML = data.is_recording ? "🔴 正在錄影" : "⚪ 未開播";

                    document.getElementById("probeText").innerText = data.probe_status;

                    // 1. 渲染磁碟進度條
                    var totalGB = data.disk_total / (1024*1024*1024);
                    var availGB = data.disk_avail / (1024*1024*1024);
                    var usedGB = data.disk_used / (1024*1024*1024);
                    var pct = totalGB > 0 ? (usedGB / totalGB) * 100 : 0;

                    document.getElementById("diskText").innerText = "已用 " + usedGB.toFixed(2) + " GB / 總共 " + totalGB.toFixed(2) + " GB (可用 " + availGB.toFixed(2) + " GB)";
                    var bar = document.getElementById("diskBarFill");
                    bar.style.width = pct.toFixed(1) + "%";
                    bar.style.backgroundColor = pct > 90 ? "var(--accent-red)" : (pct > 75 ? "var(--accent-orange)" : "var(--accent-blue)");

                    // 2. 渲染實時 CPU 數值 與 RAM 獨立進度條
                    if (data.cpu_load && data.cpu_load !== "") {
                        document.getElementById("cpuText").innerText = "(CPU: " + data.cpu_load + ")";
                    } else {
                        document.getElementById("cpuText").innerText = "(CPU: 計算中...)";
                    }

                    if (data.ram_percent !== undefined && data.ram_percent > 0) {
                        document.getElementById("ramText").innerText = "RAM: " + data.ram_percent.toFixed(1) + "%";
                        var ramBar = document.getElementById("ramBarFill");
                        ramBar.style.width = data.ram_percent.toFixed(1) + "%";
                        ramBar.style.backgroundColor = data.ram_percent > 85 ? "var(--accent-red)" : (data.ram_percent > 65 ? "var(--accent-orange)" : "var(--accent-green)");
                    }

                    // 3. 渲染實時動態寫入的錄影檔案大小
                    if (data.is_recording && data.latest_file) {
                        var targetRow = document.getElementById("row-" + data.latest_file);
                        if (targetRow) {
                            targetRow.querySelector(".file-size").innerHTML = formatBytes(data.latest_size);
                            targetRow.querySelector(".file-mtime").innerHTML = data.latest_mtime;
                        } else {
                            location.reload();
                        }
                    }
                }).catch(function(e) {
                    document.getElementById("probeText").innerHTML = "<span style='color:var(--accent-red)'>❌ 核心斷線 (OFFLINE)</span>";
                });
        }, 1000);
    </script>
</body>
</html>
`
