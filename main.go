// Yoga Chain - explorador de puntos de restauracion de Veeam Backup & Replication.
//
// Un solo binario, sin dependencias externas (solo la stdlib de Go). Sirve la
// consola web y la API en el mismo puerto. El usuario final solo ejecuta el
// binario en su laptop/workstation (Windows, Linux o macOS); no compila ni
// instala nada. Solo lectura contra la REST API de VBR.
package main

import (
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	"yogachain/internal/dbg"
	"yogachain/internal/server"
	"yogachain/internal/vbr"
)

// version del binario (se muestra en el banner de arranque y se etiqueta en el release).
const version = "0.1.3-alpha"

func main() {
	// 8001 para poder correr al lado de Yoga Benchmark (8000) en la misma maquina.
	port := flag.String("port", "8001", "puerto HTTP")
	bind := flag.String("bind", "127.0.0.1", "interfaz donde escuchar; la consola no tiene autenticacion, dejar en localhost salvo en un lab aislado")
	noBrowser := flag.Bool("no-browser", false, "no abrir el navegador automaticamente (para server headless)")
	logPath := flag.String("log", "yogachain.log", "archivo de log (para diagnostico); vacio = solo consola")
	debug := flag.Bool("debug", true, "modo debug: loguea detalle (llamadas REST, paginado); sin passwords")
	flag.Parse()
	dbg.On = *debug

	// Log a consola + archivo (para poder compartir el diagnostico). Best-effort:
	// si no se puede abrir el archivo, sigue solo por consola. NUNCA loguea
	// passwords ni tokens.
	if *logPath != "" {
		if f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
			log.SetOutput(io.MultiWriter(os.Stderr, f))
		}
	}

	// La FS embebida tiene todo bajo "frontend/"; la reraizamos ahi.
	web, err := fs.Sub(frontendFS, "frontend")
	if err != nil {
		log.Fatalf("frontend embebido: %v", err)
	}

	handler := server.New(vbr.NewStore(), web, version)

	addr := *bind + ":" + *port
	url := "http://localhost:" + *port

	// Banner de arranque (ingles): version + que esta corriendo + como abrirlo.
	fmt.Printf(`
==================================================
  Yoga Chain  v%s
  Veeam restore point explorer  (EXPERIMENTAL - lab use only, read-only)

  Server is running. Open in your browser:
      %s

  Press Ctrl+C to stop.
==================================================

`, version, url)
	log.Printf("Yoga Chain %s listening on %s (debug=%v)", version, addr, *debug)

	// Abrir el navegador solo (en desktop). En un server sin GUI, --no-browser.
	if !*noBrowser {
		go func() {
			time.Sleep(600 * time.Millisecond)
			openBrowser(url)
		}()
	}

	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatal(err)
	}
}

// openBrowser abre la URL en el navegador por defecto del sistema. Best-effort:
// si no hay GUI (server headless), falla silencioso y el server sigue igual.
func openBrowser(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	case "darwin":
		cmd, args = "open", []string{url}
	default: // linux, etc.
		cmd, args = "xdg-open", []string{url}
	}
	_ = exec.Command(cmd, args...).Start()
}
