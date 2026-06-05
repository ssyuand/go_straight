package main

import (
	"bytes"
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

// Config 設定結構體
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
	mu          sync.Mutex
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

// StreamlinkResponse 結構升級：使用 json.RawMessage 提高解析彈性
type StreamlinkResponse struct {
	Streams map[string]json.RawMessage `json:"streams"`
}

// ==============================================================================
// 🌟 核心架構升級：狀態中心化 (App Struct)
// ==============================================================================
type App struct {
	Config       Config
	State        GlobalState
	Prefix       string
	SaveDir      string
	RecordCtx    context.Context
	RecordCancel context.CancelFunc

	lastTotalTime uint64
	lastIdleTime  uint64
}

func main() {
	confFile, err := os.ReadFile("config.json")
	if err != nil {
		log.Fatalf("[ERROR] 無法讀取 config.json: %v", err)
	}
	var config Config
	if err := json.Unmarshal(confFile, &config); err != nil {
		log.Fatalf("[ERROR] 解析 config.json 失敗: %v", err)
	}

	if len(os.Args) > 1 {
		action := os.Args[1]
		switch action {
		case "status":
			showCLIStatus(config.WebPort)
			return
		case "stop", "shutdown":
			terminateService(config.WebPort)
			return
		case "start":
			if os.Getenv("LIVETOOL_DAEMON") != "1" {
				logFile, err := os.OpenFile("livetool.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
				if err != nil {
					log.Fatalf("[ERROR] 無法建立日誌檔案: %v", err)
				}
				cmd := exec.Command(os.Args[0], "start")
				cmd.Env = append(os.Environ(), "LIVETOOL_DAEMON=1")
				cmd.Stdout = logFile
				cmd.Stderr = logFile

				if err := cmd.Start(); err != nil {
					log.Fatalf("[ERROR] 自動切換至背景運行失敗: %v", err)
				}
				fmt.Printf("\x1b[32m[LAUNCH] 🚀 go_straight 核心已成功射入背景運行！\x1b[0m\n")
				fmt.Printf("📂 日誌 -> livetool.log | 📊 儀表板 -> http://localhost:%d\n", config.WebPort)
				return
			}
		default:
			fmt.Printf("未知參數: %s\n可用指令:\n  ./livetool start\n  ./livetool status\n  ./livetool stop\n", action)
			return
		}
	} else {
		fmt.Printf("提示: 請使用 ./livetool start 啟動服務。\n")
		return
	}

	parts := strings.Split(config.TargetURL, "@")
	if len(parts) < 2 {
		log.Fatalf("[ERROR] Target URL 格式錯誤，必須包含 @")
	}
	prefix := strings.Split(parts[1], "/")[0]

	cwd, _ := os.Getwd()
	saveDir := filepath.Join(cwd, "downloads", prefix)
	_ = os.MkdirAll(saveDir, 0755)

	app := &App{
		Config:  config,
		Prefix:  prefix,
		SaveDir: saveDir,
	}

	log.Printf("[SYSTEM] go_straight 啟動成功。目標頻道: @%s", app.Prefix)
	if fi, err := os.Stat(filepath.Join(cwd, "venv", "bin")); err == nil && fi.IsDir() {
		os.Setenv("PATH", filepath.Join(cwd, "venv", "bin")+":"+os.Getenv("PATH"))
	}

	app.updateDiskStatus()
	app.updateSystemResource()
	app.updateProbeStatus("⚪ 系統初始化完成，雷達待命中...")

	go app.templateProbeRadar()

	http.HandleFunc("/", app.handleIndex)
	http.HandleFunc("/api/status", app.handleAPIStatus)
	http.HandleFunc("/api/shutdown", app.handleAPIShutdown)

	addr := fmt.Sprintf(":%d", app.Config.WebPort)
	log.Printf("=========================================================")
	log.Printf("🚀 儀表板背景監聽中: http://localhost%s", addr)
	log.Printf("=========================================================")

	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("[ERROR] Web 伺服器崩潰: %v", err)
	}
}

// ==============================================================================
// 智慧雷達與「2秒極速微斷流重連防線」
// ==============================================================================
func (a *App) templateProbeRadar() {
	for {
		inWindow, timeToStart := a.checkTimeWindow()

		a.State.mu.Lock()
		isRec := a.State.IsRecording
		a.State.mu.Unlock()

		if isRec {
			a.updateProbeStatus("🟢 已交接錄影 (哨兵常駐監聽中)")
			time.Sleep(5 * time.Second)
			continue
		}

		if !inWindow {
			totalSleepSec := int(timeToStart.Seconds())
			if timeToStart >= time.Duration(a.Config.ProbeSleepDeep)*time.Second {
				totalSleepSec = a.Config.ProbeSleepDeep
			}
			for i := totalSleepSec; i > 0; i-- {
				a.State.mu.Lock()
				if a.State.IsRecording {
					a.State.mu.Unlock()
					break
				}
				a.State.mu.Unlock()

				h, m, s := i/3600, (i%3600)/60, i%60
				a.updateProbeStatus(fmt.Sprintf("💤 非戰備休眠中 (倒數 %02d:%02d:%02d)", h, m, s))
				time.Sleep(1 * time.Second)
			}
			continue
		}

		a.updateProbeStatus("🟣 發送網路請求中 (檢測開播狀態...)")
		if a.checkLiveStatus() {
			log.Printf("[PROBE] ⚠️ 發現目標 @%s 正在直播！派遣錄影核心...", a.Prefix)

			for {
				a.State.mu.Lock()
				a.State.IsRecording = true
				a.State.ProbeStatus = "🟢 已交接錄影 (哨兵常駐監聽中)"
				a.RecordCtx, a.RecordCancel = context.WithCancel(context.Background())
				a.State.mu.Unlock()

				a.runRecordEngine(a.RecordCtx)

				select {
				case <-a.RecordCtx.Done():
					goto END_RECORD
				default:
					a.updateProbeStatus("🟡 管線意外斷開，2秒後確認是否為微斷流...")
					time.Sleep(2 * time.Second)
					
					if a.checkLiveStatus() {
						log.Printf("[PROBE] 🔄 偵測到主播仍在線 (微斷流)，雷達立即接回重錄！")
						continue 
					} else {
						log.Printf("[PROBE] 🎬 主播已下播，結束錄影任務。")
						goto END_RECORD
					}
				}
			}

		END_RECORD:
			a.State.mu.Lock()
			a.State.IsRecording = false
			if a.RecordCancel != nil {
				a.RecordCancel()
			}
			a.State.mu.Unlock()
			time.Sleep(10 * time.Second)

		} else {
			waitTime := a.Config.ProbeInterval + rand.Intn(21)
			for i := waitTime; i > 0; i-- {
				a.State.mu.Lock()
				if a.State.IsRecording {
					a.State.mu.Unlock()
					break
				}
				a.State.mu.Unlock()
				a.updateProbeStatus(fmt.Sprintf("🟡 刺探待命中 (倒數 %d 秒)", i))
				time.Sleep(1 * time.Second)
			}
		}
	}
}

func (a *App) runRecordEngine(ctx context.Context) {
	tsFile := filepath.Join(a.SaveDir, time.Now().Format("20060102-150405")+".ts")
	log.Printf("[CORE] 錄影開始: %s", filepath.Base(tsFile))

	startTime := time.Now()

	streamlinkCmd := exec.CommandContext(ctx, "taskset", "-c", "0", "ionice", "-c", "2", "-n", "0",
		"streamlink", a.Config.TargetURL, "hd,ld,best",
		"--loglevel", "warning",
		"--ringbuffer-size", "512M",
		"--stream-segment-threads", "1",
		"--stream-timeout", "60",
		"--http-header", "Referer=https://www.tiktok.com/",
		"--http-header", "Origin=https://www.tiktok.com",
		"--http-header", "User-Agent=Mozilla/5.0 (X11; Linux x86_64; rv:126.0) Gecko/20100101 Firefox/126.0",
		"-O",
	)

	ffmpegCmd := exec.CommandContext(ctx, "taskset", "-c", "0", "ionice", "-c", "2", "-n", "0",
		"ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-i", "pipe:0", "-c", "copy", "-f", "mpegts", tsFile,
	)

	streamlinkCmd.Env = append(os.Environ(), "LD_PRELOAD=/usr/lib/libjemalloc.so")
	ffmpegCmd.Env = append(os.Environ(), "LD_PRELOAD=/usr/lib/libjemalloc.so")

	var streamlinkStderr bytes.Buffer
	var ffmpegStderr bytes.Buffer
	streamlinkCmd.Stderr = &streamlinkStderr
	ffmpegCmd.Stderr = &ffmpegStderr

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
		log.Printf("[CORE] FFmpeg 啟動失敗: %v", err)
		return
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				fi, err := os.Stat(tsFile)
				if err == nil {
					a.State.mu.Lock()
					a.State.LatestFile = filepath.Base(tsFile)
					a.State.LatestMTime = fi.ModTime().Format("2006-01-02 15:04:05")
					a.State.LatestSize = fi.Size()
					a.State.mu.Unlock()
				}
				time.Sleep(1 * time.Second)
			}
		}
	}()

	errStreamlink := streamlinkCmd.Wait()
	errFfmpeg := ffmpegCmd.Wait()
	lifespan := time.Since(startTime)

	select {
	case <-ctx.Done():
		log.Printf("[CORE] 錄影收到中斷指令，已主動停止。時長: %d 秒", int(lifespan.Seconds()))
	default:
		if errStreamlink != nil || errFfmpeg != nil {
			log.Printf("[ALARM] ⚠️ 管線異常中斷！產生診斷報告...")

			if errStreamlink != nil {
				exitCode := -1
				if exitErr, ok := errStreamlink.(*exec.ExitError); ok {
					exitCode = exitErr.ExitCode()
				}
				log.Printf("[DIAGNOSIS] streamlink 異常 (Exit: %d): %v", exitCode, errStreamlink)
				if streamlinkStderr.Len() > 0 {
					log.Printf("[DIAGNOSIS] streamlink 遺言:\n%s", strings.TrimSpace(streamlinkStderr.String()))
				}
			}

			if errFfmpeg != nil {
				exitCode := -1
				if exitErr, ok := errFfmpeg.(*exec.ExitError); ok {
					exitCode = exitErr.ExitCode()
				}
				log.Printf("[DIAGNOSIS] ffmpeg 異常 (Exit: %d): %v", exitCode, errFfmpeg)
				if ffmpegStderr.Len() > 0 {
					log.Printf("[DIAGNOSIS] ffmpeg 遺言:\n%s", strings.TrimSpace(ffmpegStderr.String()))
				}
			}
		}
	}

	fi, err := os.Stat(tsFile)
	if err != nil || fi.Size() == 0 || lifespan < 5*time.Second {
		_ = os.Remove(tsFile)
		log.Printf("[CORE] 移除垃圾碎片 (%d 秒)。", int(lifespan.Seconds()))
	} else {
		log.Printf("[CORE] 碎片儲存成功 (%d 秒)。", int(lifespan.Seconds()))
	}
}

// ==============================================================================
// 輔助與系統資源方法 (綁定至 App)
// ==============================================================================
func (a *App) updateProbeStatus(status string) {
	a.State.mu.Lock()
	a.State.ProbeStatus = status
	a.State.mu.Unlock()
}

// 智慧型開播狀態檢測 (使用 JSON 解析)
func (a *App) checkLiveStatus() bool {
	cmd := exec.Command("streamlink", "--json", a.Config.TargetURL)
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	var res StreamlinkResponse
	if err := json.Unmarshal(output, &res); err != nil {
		log.Printf("[PROBE] JSON 解析失敗: %v", err)
		return false
	}

	// 只要 Streams Map 有內容，代表成功抓取到串流，視為直播中
	return len(res.Streams) > 0
}

func (a *App) checkTimeWindow() (bool, time.Duration) {
	if a.Config.ProbeStart == a.Config.ProbeEnd {
		return true, 0
	}
	now := time.Now()
	todayStr := now.Format("2006-01-02")
	start, _ := time.ParseInLocation("2006-01-02 15:04", todayStr+" "+a.Config.ProbeStart, time.Local)
	end, _ := time.ParseInLocation("2006-01-02 15:04", todayStr+" "+a.Config.ProbeEnd, time.Local)

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

func (a *App) updateDiskStatus() {
	cmd := exec.Command("df", "-B1", a.SaveDir)
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

	a.State.mu.Lock()
	a.State.DiskTotal = total
	a.State.DiskUsed = used
	a.State.DiskAvail = avail
	a.State.mu.Unlock()
}

func (a *App) updateSystemResource() {
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

				a.State.mu.Lock()
				if a.lastTotalTime > 0 && currentTotal > a.lastTotalTime {
					totalDiff := currentTotal - a.lastTotalTime
					idleDiff := currentIdle - a.lastIdleTime
					cpuPercent := (float64(totalDiff-idleDiff) / float64(totalDiff)) * 100
					a.State.CPULoad = fmt.Sprintf("%.1f%%", cpuPercent)
				} else {
					a.State.CPULoad = "計算中..."
				}
				a.State.mu.Unlock()

				a.lastTotalTime = currentTotal
				a.lastIdleTime = currentIdle
			}
		}
	}

	memData, err := os.ReadFile("/proc/meminfo")
	if err == nil {
		var memTotal, memAvail float64
		lines := strings.Split(string(memData), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "MemTotal:") {
				fields := strings.Fields(line)
				if len(fields) > 1 { memTotal, _ = strconv.ParseFloat(fields[1], 64) }
			}
			if strings.HasPrefix(line, "MemAvailable:") {
				fields := strings.Fields(line)
				if len(fields) > 1 { memAvail, _ = strconv.ParseFloat(fields[1], 64) }
				break
			}
		}
		if memTotal > 0 {
			a.State.mu.Lock()
			a.State.RAMPercent = ((memTotal - memAvail) / memTotal) * 100
			a.State.mu.Unlock()
		}
	}
}

// ==============================================================================
// 網頁與 API 伺服器
// ==============================================================================
func (a *App) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	a.updateDiskStatus()
	a.updateSystemResource()

	a.State.mu.Lock()
	defer a.State.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(a.State)
}

func (a *App) handleAPIShutdown(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"shutting_down"}`))
	log.Println("[SYSTEM] 收到遠端停止訊號，進程正優雅關閉中...")

	a.State.mu.Lock()
	if a.RecordCancel != nil {
		a.RecordCancel()
	}
	a.State.mu.Unlock()

	_ = exec.Command("pkill", "-f", "streamlink").Run()
	_ = exec.Command("pkill", "-f", "ffmpeg").Run()

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

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/download/") {
		fileName := strings.TrimPrefix(r.URL.Path, "/download/")
		http.ServeFile(w, r, filepath.Join(a.SaveDir, fileName))
		return
	}

	files, _ := os.ReadDir(a.SaveDir)
	var rows []FileRow

	a.State.mu.Lock()
	activeFile := ""
	if a.State.IsRecording {
		activeFile = a.State.LatestFile
	}
	a.State.mu.Unlock()

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

	tmpl, _ := template.New("index").Parse(htmlTemplate)
	data := PageData{
		Prefix:     a.Prefix,
		ProbeStart: a.Config.ProbeStart,
		ProbeEnd:   a.Config.ProbeEnd,
		Files:      rows,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = tmpl.Execute(w, data)
}

func showCLIStatus(port int) {
	url := fmt.Sprintf("http://localhost:%d/api/status", port)
	resp, err := http.Get(url)
	if err != nil {
		fmt.Println("\x1b[31m[OFFLINE] 核心服務未啟動！\x1b[0m")
		return
	}
	defer resp.Body.Close()

	var s GlobalState
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return
	}
	gb := float64(1024 * 1024 * 1024)
	pct := 0.0
	if s.DiskTotal > 0 { pct = (float64(s.DiskUsed) / float64(s.DiskTotal)) * 100 }

	fmt.Println("\x1b[36m=========================================================\x1b[0m")
	if s.IsRecording {
		fmt.Println(" 🎬 錄影狀態: \x1b[31m● 錄影中 (RECORDING)\x1b[0m")
		fmt.Printf(" 📂 當前檔案: %s\n", s.LatestFile)
		fmt.Printf(" ⚖️ 檔案大小: %.2f MB\n", float64(s.LatestSize)/(1024*1024))
	} else {
		fmt.Println(" 🎬 錄影狀態: \x1b[90m○ 未開播 (IDLE)\x1b[0m")
	}
	fmt.Printf(" 📡 雷達狀態: %s\n", s.ProbeStatus)
	fmt.Printf(" 💾 磁碟空間: 已用 %.2f GB / 總共 %.2f GB (使用 %.1f%%)\n", float64(s.DiskUsed)/gb, float64(s.DiskTotal)/gb, pct)
	fmt.Printf(" ⚡ 系統效能: CPU: %s | RAM: %.1f%%\n", s.CPULoad, s.RAMPercent)
	fmt.Println("\x1b[36m=========================================================\x1b[0m")
}

func terminateService(port int) {
	url := fmt.Sprintf("http://localhost:%d/api/shutdown", port)
	http.Get(url)
	_ = exec.Command("pkill", "-9", "-f", "streamlink").Run()
	_ = exec.Command("pkill", "-9", "-f", "ffmpeg").Run()
	time.Sleep(300 * time.Millisecond)
	_ = exec.Command("pkill", "-9", "-f", "livetool start").Run()
	fmt.Println("\x1b[32m[CLEAN] ✨ 背景所有殘留程序已徹底清空！\x1b[0m")
}

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
        body { background-color: var(--bg-main); color: var(--text-main); font-family: -apple-system, sans-serif; margin: 0; padding: 30px; display: flex; justify-content: center; }
        .container { width: 100%; max-width: 1000px; }
        header { display: flex; justify-content: space-between; align-items: center; margin-bottom: 25px; }
        h2 { margin: 0; font-size: 24px; font-weight: 700; }
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
            <div class="progress-bg"><div id="diskBarFill" class="progress-fill"></div></div>
            <div class="meta-row">
                <span class="meta-label">⚡ 系統實時負載 <span id="cpuText" style="color:var(--text-main); font-family:monospace; margin-left:5px;">(CPU: 讀取中...)</span></span>
                <span id="ramText" class="meta-value">RAM: 讀取中...</span>
            </div>
            <div class="progress-bg"><div id="ramBarFill" class="progress-fill" style="background-color: var(--accent-green);"></div></div>
            <div class="divider"></div>
            <div class="meta-row" style="margin-bottom:0; padding-top:4px;">
                <span class="meta-label">📡 背景雷達狀態 <span style="font-size:12px; color:#444;">(排程窗: {{.ProbeStart}} ~ {{.ProbeEnd}})</span></span>
                <span id="probeText" class="meta-value" style="color: var(--accent-orange);">連線中...</span>
            </div>
        </div>
        <h3 style="font-size:16px; color:var(--text-muted); margin-bottom:12px; font-weight:600;">📂 已錄製歷史片段</h3>
        <table id="vidTable">
            <thead><tr><th>檔案名稱</th><th>容量大小</th><th>最後修改時間</th></tr></thead>
            <tbody>
                {{range .Files}}
                <tr id="row-{{.Name}}" class="{{if .IsGrowing}}row-growing{{end}}">
                    <td>{{if .IsGrowing}}<span class="pulse-dot" style="margin-right:6px;"></span>{{end}}<a class="file-link" href="/download/{{.Name}}" download>{{.Name}}</a></td>
                    <td class="file-size" data-bytes="{{.SizeBytes}}">讀取中...</td><td class="file-mtime">{{.MTime}}</td>
                </tr>
                {{else}}
                <tr><td colspan="3" style="text-align:center; color:var(--text-muted); padding:30px;">目前尚無任何錄影檔案。雷達正在暗中為您守候...</td></tr>
                {{end}}
            </tbody>
        </table>
    </div>
    <script>
        function formatBytes(bytes) {
            if (bytes === 0) return "0.00 MB";
            var k = 1024, sizes = ['Bytes', 'KB', 'MB', 'GB', 'TB'], i = Math.floor(Math.log(bytes) / Math.log(k));
            if (i < 2) i = 2; 
            var val = bytes / Math.pow(k, i);
            return (sizes[i] === 'GB' || sizes[i] === 'TB') ? "<b>" + val.toFixed(2) + " " + sizes[i] + "</b>" : val.toFixed(2) + " " + sizes[i];
        }
        document.querySelectorAll('.file-size').forEach(function(td) {
            var b = parseInt(td.getAttribute('data-bytes'));
            if(!isNaN(b)) td.innerHTML = formatBytes(b);
        });
        setInterval(function() {
            fetch('/api/status').then(r => r.json()).then(data => {
                var badge = document.getElementById("liveBadge");
                badge.className = data.is_recording ? "badge badge-live" : "badge badge-offline";
                badge.innerHTML = data.is_recording ? "🔴 正在錄影" : "⚪ 未開播";
                document.getElementById("probeText").innerText = data.probe_status;

                var totalGB = data.disk_total / (1024*1024*1024), availGB = data.disk_avail / (1024*1024*1024), usedGB = data.disk_used / (1024*1024*1024);
                var pct = totalGB > 0 ? (usedGB / totalGB) * 100 : 0;
                document.getElementById("diskText").innerText = "已用 " + usedGB.toFixed(2) + " GB / 總共 " + totalGB.toFixed(2) + " GB (可用 " + availGB.toFixed(2) + " GB)";
                var bar = document.getElementById("diskBarFill");
                bar.style.width = pct.toFixed(1) + "%";
                bar.style.backgroundColor = pct > 90 ? "var(--accent-red)" : (pct > 75 ? "var(--accent-orange)" : "var(--accent-blue)");

                document.getElementById("cpuText").innerText = "(CPU: " + (data.cpu_load || "計算中...") + ")";
                if (data.ram_percent !== undefined && data.ram_percent > 0) {
                    document.getElementById("ramText").innerText = "RAM: " + data.ram_percent.toFixed(1) + "%";
                    var ramBar = document.getElementById("ramBarFill");
                    ramBar.style.width = data.ram_percent.toFixed(1) + "%";
                    ramBar.style.backgroundColor = data.ram_percent > 85 ? "var(--accent-red)" : (data.ram_percent > 65 ? "var(--accent-orange)" : "var(--accent-green)");
                }

                if (data.is_recording && data.latest_file) {
                    var targetRow = document.getElementById("row-" + data.latest_file);
                    if (targetRow) {
                        targetRow.querySelector(".file-size").innerHTML = formatBytes(data.latest_size);
                        targetRow.querySelector(".file-mtime").innerHTML = data.latest_mtime;
                    } else { location.reload(); }
                }
            }).catch(e => { document.getElementById("probeText").innerHTML = "<span style='color:var(--accent-red)'>❌ 核心斷線 (OFFLINE)</span>"; });
        }, 1000);
    </script>
</body>
</html>
`
