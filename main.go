package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

const usage = `Usage: pteron <command> [arguments] [options]

  publish [file|-] [--title TITLE] [--id ID] [--json]
  update <id> [file|-] [--title TITLE] [--json]
  feedback <id> [--clear] [--json]
  list [--json]
  rm <id> [id...]
  serve [--port PORT] [--data-dir DIR]

All commands accept --port (default: PTERON_PORT or 8848).
Data directory: --data-dir, PTERON_DATA_DIR, or ~/.local/share/pteron.
Publish and update read stdin when the file is omitted or '-'.
`

type options struct {
	port               int
	dataDir, title, id string
	json, clear        bool
}

// Standard flag stops at the first positional argument. Move recognized options
// forward so both "publish file --title ..." and global flags work naturally.
func parseArgs(args []string) (options, []string, error) {
	var opts options
	opts.port = 8848
	if value := os.Getenv("PTERON_PORT"); value != "" {
		port, err := strconv.Atoi(value)
		if err != nil {
			return opts, nil, fmt.Errorf("invalid PTERON_PORT: %q", value)
		}
		opts.port = port
	}
	opts.dataDir = os.Getenv("PTERON_DATA_DIR")
	if opts.dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return opts, nil, err
		}
		opts.dataDir = filepath.Join(home, ".local", "share", "pteron")
	}
	flags := flag.NewFlagSet("pteron", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.IntVar(&opts.port, "port", opts.port, "HTTP port")
	flags.StringVar(&opts.dataDir, "data-dir", opts.dataDir, "data directory")
	flags.StringVar(&opts.title, "title", "", "page title")
	flags.StringVar(&opts.id, "id", "", "site ID")
	flags.BoolVar(&opts.json, "json", false, "print JSON")
	flags.BoolVar(&opts.clear, "clear", false, "clear feedback")
	var positional, reordered []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		reordered = append(reordered, arg)
		name := strings.TrimLeft(arg, "-")
		if !strings.Contains(name, "=") && (name == "port" || name == "data-dir" || name == "title" || name == "id") {
			i++
			if i >= len(args) {
				return opts, nil, fmt.Errorf("%s requires a value", arg)
			}
			reordered = append(reordered, args[i])
		}
	}
	if err := flags.Parse(reordered); err != nil {
		return opts, nil, err
	}
	if opts.port < 1 || opts.port > 65535 {
		return opts, nil, errors.New("port must be between 1 and 65535")
	}
	return opts, positional, nil
}
func readHTML(files []string) (string, error) {
	if len(files) > 1 {
		return "", errors.New("expected at most one HTML file")
	}
	var reader io.Reader = os.Stdin
	if len(files) == 1 && files[0] != "-" {
		file, err := os.Open(files[0])
		if err != nil {
			return "", err
		}
		defer file.Close()
		reader = file
	} else if len(files) == 0 {
		stat, err := os.Stdin.Stat()
		if err != nil {
			return "", err
		}
		if stat.Mode()&os.ModeCharDevice != 0 {
			return "", errors.New("provide an HTML file or pipe HTML to stdin")
		}
	}
	content, err := io.ReadAll(io.LimitReader(reader, (16<<20)+1))
	if err != nil {
		return "", err
	}
	if len(content) > 16<<20 {
		return "", errors.New("HTML exceeds 16 MiB")
	}
	return string(content), nil
}
func apiRequest(port int, method, path string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach pteron on port %d; start pteron.service or run 'pteron serve': %w", port, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= 400 {
		var message struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &message) == nil && message.Error != "" {
			return nil, fmt.Errorf("%s: %s", response.Status, message.Error)
		}
		return nil, fmt.Errorf("%s: %s", response.Status, strings.TrimSpace(string(data)))
	}
	return data, nil
}
func printSite(data []byte, raw bool) error {
	if raw {
		fmt.Println(strings.TrimSpace(string(data)))
		return nil
	}
	var site Site
	if err := json.Unmarshal(data, &site); err != nil {
		return err
	}
	fmt.Printf("ID:    %s\nTitle: %s\nURL:   %s\n", site.ID, site.Title, site.URL)
	return nil
}
func cli(args []string) error {
	opts, positional, err := parseArgs(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Print(usage)
		return nil
	}
	if err != nil {
		return err
	}
	if len(positional) == 0 {
		fmt.Print(usage)
		return nil
	}
	command, rest := positional[0], positional[1:]
	switch command {
	case "help":
		fmt.Print(usage)
		return nil
	case "serve":
		if len(rest) != 0 {
			return errors.New("usage: pteron serve [--port PORT] [--data-dir DIR]")
		}
		return serve(opts.dataDir, opts.port)
	case "publish", "update":
		method, path := http.MethodPost, "/api/sites"
		if command == "update" {
			if len(rest) == 0 {
				return errors.New("usage: pteron update <id> [file|-]")
			}
			if !validID.MatchString(rest[0]) {
				return errInvalidID
			}
			method, path = http.MethodPut, path+"/"+rest[0]
			rest = rest[1:]
		}
		if opts.id != "" && !validID.MatchString(opts.id) {
			return errInvalidID
		}
		source, err := readHTML(rest)
		if err != nil {
			return err
		}
		data, err := apiRequest(opts.port, method, path, map[string]string{"id": opts.id, "title": opts.title, "html": source})
		if err != nil {
			return err
		}
		return printSite(data, opts.json)
	case "feedback":
		if len(rest) != 1 {
			return errors.New("usage: pteron feedback <id> [--clear] [--json]")
		}
		if !validID.MatchString(rest[0]) {
			return errInvalidID
		}
		method := http.MethodGet
		if opts.clear {
			method = http.MethodDelete
		}
		data, err := apiRequest(opts.port, method, "/api/sites/"+rest[0]+"/feedback", nil)
		if err != nil {
			return err
		}
		if opts.clear && !opts.json {
			fmt.Printf("Feedback cleared: %s\n", rest[0])
			return nil
		}
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, data, "", "  "); err != nil {
			return err
		}
		fmt.Println(strings.TrimSpace(pretty.String()))
		return nil
	case "list":
		if len(rest) != 0 {
			return errors.New("usage: pteron list [--json]")
		}
		data, err := apiRequest(opts.port, http.MethodGet, "/api/sites", nil)
		if err != nil {
			return err
		}
		if opts.json {
			fmt.Println(strings.TrimSpace(string(data)))
			return nil
		}
		var sites []Site
		if err := json.Unmarshal(data, &sites); err != nil {
			return err
		}
		writer := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "ID\tUPDATED\tFEEDBACK\tTITLE\tURL")
		for _, site := range sites {
			feedback := fmt.Sprintf("%d fields", len(site.Feedback.Fields))
			if site.Feedback.Submitted {
				feedback += " (submitted)"
			}
			title := strings.Join(strings.Fields(site.Title), " ")
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", site.ID, site.UpdatedAt, feedback, title, site.URL)
		}
		return writer.Flush()
	case "rm":
		if len(rest) == 0 {
			return errors.New("usage: pteron rm <id> [id...]")
		}
		for _, id := range rest {
			if !validID.MatchString(id) {
				return errInvalidID
			}
		}
		for _, id := range rest {
			if _, err := apiRequest(opts.port, http.MethodDelete, "/api/sites/"+id, nil); err != nil {
				return err
			}
			fmt.Printf("Deleted: %s\n", id)
		}
		return nil
	default:
		return fmt.Errorf("unknown command %q; run 'pteron --help'", command)
	}
}
func main() {
	if err := cli(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "pteron:", err)
		os.Exit(1)
	}
}
