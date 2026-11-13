//go:build windows

// Command openvms-tray shows OpenVMS Recorder's state in the Windows
// notification area and controls the service. Build with
// -ldflags "-H windowsgui" so no console window opens.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fyne.io/systray"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/jdolan-exalink/openvms-rec/internal/config"
	"github.com/jdolan-exalink/openvms-rec/internal/platform/winsvc"
)

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const runValue = "OpenVMSRecorderTray"

var (
	defaultConfig = filepath.Join(os.Getenv("ProgramData"), "OpenVMS", "Recorder", "recorder.yaml")
	flagURL       = flag.String("url", "", "recorder base URL (default: from the configuration, else http://127.0.0.1:8640)")
	flagConfig    = flag.String("config", defaultConfig, "recorder configuration, used for the URL and folders")
	flagSvc       = flag.String("svc", "", "internal: start|stop|restart the service (used for elevation)")
)

func main() {
	flag.Parse()
	if *flagSvc != "" {
		// Elevated helper: do one service action and exit.
		if err := winsvc.Control(*flagSvc); err != nil {
			messageBox("OpenVMS Recorder", "No se pudo "+actionES(*flagSvc)+" el servicio:\n"+err.Error(), true)
			os.Exit(1)
		}
		return
	}
	if !singleInstance() {
		return
	}
	systray.Run(onReady, func() {})
}

type settings struct {
	base       string
	dataDir    string
	recordings string
}

func loadSettings() settings {
	s := settings{base: "http://127.0.0.1:8640", dataDir: filepath.Dir(*flagConfig)}
	if cfg, err := config.Load(*flagConfig); err == nil {
		host := cfg.Server.Listen
		if strings.HasPrefix(host, "0.0.0.0:") || strings.HasPrefix(host, ":") || strings.HasPrefix(host, "[::]:") {
			host = "127.0.0.1:" + host[strings.LastIndex(host, ":")+1:]
		}
		s.base = "http://" + host
		if cfg.Data.Dir != "" {
			s.dataDir = cfg.Data.Dir
		}
		for _, v := range cfg.Storage.Volumes {
			if p := v.PathWindows; p != "" {
				s.recordings = p
				break
			}
			if v.Path != "" {
				s.recordings = v.Path
				break
			}
		}
	}
	if *flagURL != "" {
		s.base = strings.TrimRight(*flagURL, "/")
	}
	return s
}

func onReady() {
	st := loadSettings()
	systray.SetIcon(iconICO(colorStopped))
	systray.SetTitle("OpenVMS Recorder")
	systray.SetTooltip("OpenVMS Recorder")

	mTitle := systray.AddMenuItem("Consultando…", "")
	mTitle.Disable()
	mDetail := systray.AddMenuItem("", "")
	mDetail.Disable()
	systray.AddSeparator()
	mOpen := systray.AddMenuItem("Abrir panel", "Abre la UI web del recorder")
	mRes := systray.AddMenuItem("Ver recursos y errores", "CPU, memoria, discos y últimos errores")
	systray.AddSeparator()
	mStart := systray.AddMenuItem("Iniciar servicio", "")
	mStop := systray.AddMenuItem("Detener servicio", "Deja de grabar todas las cámaras")
	mRestart := systray.AddMenuItem("Reiniciar servicio", "")
	systray.AddSeparator()
	mRec := systray.AddMenuItem("Abrir carpeta de grabaciones", "")
	mData := systray.AddMenuItem("Abrir carpeta de datos y logs", "")
	mAuto := systray.AddMenuItemCheckbox("Iniciar con Windows", "Mostrar este ícono al iniciar sesión", autostart())
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Salir del panel", "Cierra solo el ícono; el servicio sigue grabando")
	if st.recordings == "" {
		mRec.Hide()
	}

	refresh := make(chan struct{}, 1)
	poke := func() {
		select {
		case refresh <- struct{}{}:
		default:
		}
	}
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		last := ""
		for {
			svcState, installed, _ := winsvc.State()
			if !installed {
				svcState = ""
			}
			h, err := fetchHealth(context.Background(), st.base)
			v := render(svcState, h, err)
			key := fmt.Sprint(v.Color)
			if key != last {
				systray.SetIcon(iconICO(v.Color))
				last = key
			}
			systray.SetTooltip(v.Tooltip)
			mTitle.SetTitle(v.Title)
			if v.Detail == "" {
				mDetail.Hide()
			} else {
				mDetail.SetTitle(v.Detail)
				mDetail.Show()
			}
			setEnabled(mOpen, h != nil)
			setEnabled(mRes, h != nil)
			if installed {
				mStart.Show()
				mStop.Show()
				mRestart.Show()
				setEnabled(mStart, svcState == "stopped")
				setEnabled(mStop, svcState == "running")
				setEnabled(mRestart, svcState == "running")
			} else {
				mStart.Hide()
				mStop.Hide()
				mRestart.Hide()
			}
			select {
			case <-t.C:
			case <-refresh:
			}
		}
	}()

	go func() {
		for {
			select {
			case <-mOpen.ClickedCh:
				shellOpen(st.base + "/")
			case <-mRes.ClickedCh:
				shellOpen(st.base + "/#sistema")
			case <-mStart.ClickedCh:
				go serviceAction("start", poke)
			case <-mStop.ClickedCh:
				if confirm("¿Detener el servicio? Se deja de grabar todas las cámaras hasta que lo vuelvas a iniciar.") {
					go serviceAction("stop", poke)
				}
			case <-mRestart.ClickedCh:
				go serviceAction("restart", poke)
			case <-mRec.ClickedCh:
				shellOpen(st.recordings)
			case <-mData.ClickedCh:
				shellOpen(st.dataDir)
			case <-mAuto.ClickedCh:
				on := !mAuto.Checked()
				if err := setAutostart(on); err == nil {
					if on {
						mAuto.Check()
					} else {
						mAuto.Uncheck()
					}
				}
			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}

func setEnabled(m *systray.MenuItem, on bool) {
	if on {
		m.Enable()
	} else {
		m.Disable()
	}
}

// serviceAction tries directly and, when Windows says the user lacks the
// rights, relaunches this program elevated (UAC prompt) for that action.
func serviceAction(action string, done func()) {
	defer done()
	err := winsvc.Control(action)
	if errors.Is(err, winsvc.ErrAccessDenied) {
		exe, _ := os.Executable()
		verb, _ := windows.UTF16PtrFromString("runas")
		file, _ := windows.UTF16PtrFromString(exe)
		args, _ := windows.UTF16PtrFromString("-svc " + action)
		if err := windows.ShellExecute(0, verb, file, args, nil, windows.SW_HIDE); err != nil {
			return // UAC declined
		}
		time.Sleep(2 * time.Second)
		return
	}
	if err != nil {
		messageBox("OpenVMS Recorder", "No se pudo "+actionES(action)+" el servicio:\n"+err.Error(), true)
	}
}

func actionES(a string) string {
	return map[string]string{"start": "iniciar", "stop": "detener", "restart": "reiniciar"}[a]
}

func shellOpen(target string) {
	verb, _ := windows.UTF16PtrFromString("open")
	t, _ := windows.UTF16PtrFromString(target)
	windows.ShellExecute(0, verb, t, nil, nil, windows.SW_SHOWNORMAL)
}

func autostart() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(runValue)
	return err == nil
}

func setAutostart(on bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		return k.DeleteValue(runValue)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := fmt.Sprintf("%q", exe)
	if *flagConfig != defaultConfig {
		cmd += fmt.Sprintf(" -config %q", *flagConfig)
	}
	if *flagURL != "" {
		cmd += fmt.Sprintf(" -url %q", *flagURL)
	}
	return k.SetStringValue(runValue, cmd)
}

func singleInstance() bool {
	name, _ := windows.UTF16PtrFromString(`Local\OpenVMSRecorderTray`)
	_, err := windows.CreateMutex(nil, false, name)
	return !errors.Is(err, windows.ERROR_ALREADY_EXISTS)
}

var (
	user32     = windows.NewLazySystemDLL("user32.dll")
	procMsgBox = user32.NewProc("MessageBoxW")
)

const (
	mbOK          = 0x0
	mbYesNo       = 0x4
	mbIconWarning = 0x30
	mbIconError   = 0x10
	idYes         = 6
)

func messageBox(title, text string, isErr bool) int {
	t, _ := windows.UTF16PtrFromString(title)
	m, _ := windows.UTF16PtrFromString(text)
	flags := uintptr(mbOK)
	if isErr {
		flags |= mbIconError
	}
	r, _, _ := procMsgBox.Call(0, uintptrOf(m), uintptrOf(t), flags)
	return int(r)
}

func confirm(text string) bool {
	t, _ := windows.UTF16PtrFromString("OpenVMS Recorder")
	m, _ := windows.UTF16PtrFromString(text)
	r, _, _ := procMsgBox.Call(0, uintptrOf(m), uintptrOf(t), mbYesNo|mbIconWarning)
	return r == idYes
}
