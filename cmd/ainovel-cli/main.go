package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/voocel/ainovel-cli/assets"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/entry/headless"
	"github.com/voocel/ainovel-cli/internal/entry/startup"
	"github.com/voocel/ainovel-cli/internal/entry/tui"
	"github.com/voocel/ainovel-cli/internal/entry/web"
	"github.com/voocel/ainovel-cli/internal/eval"
	"github.com/voocel/ainovel-cli/internal/rules"
	buildversion "github.com/voocel/ainovel-cli/internal/version"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

// headlessMode ghi nhận chế độ khởi động headless.
var headlessMode bool

func main() {
	// Kiểm tra lệnh con eval trước khi parse flag
	if len(os.Args) > 1 && os.Args[1] == "eval" {
		os.Exit(eval.Command(os.Args[2:]))
	}

	opts, args, err := parseCLIOptions(os.Args[1:])
	if err != nil {
		die("Lỗi tham số: %v", err)
	}
	if opts.Version {
		buildversion.Print(os.Stdout, versionInfo())
		return
	}
	if opts.Update {
		if err := runSelfUpdate(opts.UpdateVersion); err != nil {
			fmt.Fprintf(os.Stderr, "Cập nhật thất bại: %v\n", err)
			os.Exit(1)
		}
		return
	}
	headlessMode = opts.Headless

	// Khởi tạo lần đầu nếu chưa có cấu hình
	if bootstrap.NeedsSetup() {
		if opts.Headless || opts.Web {
			die("Lỗi: Chế độ headless không hỗ trợ thiết lập lần đầu, vui lòng chạy chế độ TUI trước để hoàn tất cấu hình.")
		}
		setupCfg, err := bootstrap.RunSetup()
		if err != nil {
			die("Thiết lập thất bại: %v", err)
		}
		runWithConfig(setupCfg, opts, args)
		return
	}

	// Đọc cấu hình
	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		die("Lỗi cấu hình: %v", err)
	}

	runWithConfig(cfg, opts, args)
}

// die xử lý thoát khi có lỗi nghiêm trọng: in ra stderr, lưu log và tạm dừng nếu ở terminal tương tác.
func die(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintln(os.Stderr, msg)
	if path := bootstrap.WriteStartupError(msg); path != "" {
		fmt.Fprintf(os.Stderr, " (Chi tiết lỗi đã được ghi vào: %s)\n", path)
	}
	if !headlessMode && stdinIsTerminal() {
		fmt.Fprint(os.Stderr, "\nNhấn Enter để thoát...")
		fmt.Fscanln(os.Stdin)
	}
	os.Exit(1)
}

func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func runWithConfig(cfg bootstrap.Config, opts cliOptions, args []string) {
	rules.EnsureHomeRulesDir()

	if len(args) > 0 {
		die("Lỗi: Không hỗ trợ truyền trực tiếp yêu cầu truyện qua dòng lệnh, vui lòng nhập vào ô chat trong giao diện TUI sau khi khởi động.")
	}

	cfg.FillDefaults()
	bundle := assets.LoadWithLanguage(cfg.NormalizedLanguage(), cfg.Style, assets.DefaultLoadOptions(cfg.OutputDir))
	if opts.Headless {
		prompt, err := loadPrompt(opts)
		if err != nil {
			die("Lỗi: %v", err)
		}
		if err := headless.Run(cfg, bundle, headless.Options{Prompt: prompt}); err != nil {
			die("Lỗi: %v", err)
		}
		return
	}
	if opts.Prompt != "" || opts.PromptFile != "" {
		die("Lỗi: Tham số --prompt / --prompt-file chỉ có thể sử dụng trong chế độ --headless.")
	}
	if opts.Web {
		if err := web.Run(cfg, web.Options{Addr: opts.Addr}); err != nil {
			die("Lỗi: %v", err)
		}
		return
	}
	if err := tui.Run(cfg, bundle, versionInfo()); err != nil {
		die("Lỗi: %v", err)
	}
}

type cliOptions struct {
	Headless      bool
	Web           bool
	Addr          string
	Prompt        string
	PromptFile    string
	Version       bool
	Update        bool
	UpdateVersion string
}

func parseCLIOptions(argv []string) (cliOptions, []string, error) {
	var opts cliOptions
	var args []string
	for i := 0; i < len(argv); i++ {
		switch argv[i] {
		case "--version", "-v":
			opts.Version = true
		case "version":
			if i+1 < len(argv) {
				return opts, nil, fmt.Errorf("lệnh version không nhận thêm tham số")
			}
			opts.Version = true
		case "update":
			if opts.Update {
				return opts, nil, fmt.Errorf("lệnh update chỉ có thể chỉ định một lần")
			}
			opts.Update = true
			if i+1 < len(argv) {
				if strings.HasPrefix(argv[i+1], "-") {
					return opts, nil, fmt.Errorf("lệnh update chỉ nhận một tham số phiên bản tùy chọn")
				}
				opts.UpdateVersion = argv[i+1]
				i++
			}
			if i+1 < len(argv) {
				return opts, nil, fmt.Errorf("lệnh update chỉ nhận một tham số phiên bản tùy chọn")
			}
		case "--headless":
			opts.Headless = true
		case "--web":
			opts.Web = true
		case "--addr":
			if i+1 >= len(argv) {
				return opts, nil, fmt.Errorf("--addr thiếu giá trị")
			}
			opts.Addr = argv[i+1]
			i++
		case "--prompt":
			if i+1 >= len(argv) {
				return opts, nil, fmt.Errorf("--prompt thiếu giá trị")
			}
			opts.Prompt = argv[i+1]
			i++
		case "--prompt-file":
			if i+1 >= len(argv) {
				return opts, nil, fmt.Errorf("--prompt-file thiếu giá trị")
			}
			opts.PromptFile = argv[i+1]
			i++
		default:
			args = append(args, argv[i])
		}
	}
	if opts.Prompt != "" && opts.PromptFile != "" {
		return opts, nil, fmt.Errorf("--prompt và --prompt-file không thể dùng đồng thời")
	}
	if opts.Web && opts.Headless {
		return opts, nil, fmt.Errorf("--web và --headless không thể dùng đồng thời")
	}
	if opts.Addr != "" && !opts.Web {
		return opts, nil, fmt.Errorf("--addr chỉ dùng cùng --web")
	}
	if opts.Web && opts.Addr == "" {
		opts.Addr = ":8080"
	}
	if opts.Version && (opts.Update || opts.Headless || opts.Prompt != "" || opts.PromptFile != "" || len(args) > 0) {
		return opts, nil, fmt.Errorf("version không thể dùng chung với các tham số khởi động khác")
	}
	if opts.Update && (opts.Headless || opts.Prompt != "" || opts.PromptFile != "" || len(args) > 0) {
		return opts, nil, fmt.Errorf("update không thể dùng chung với các tham số khởi động khác")
	}
	return opts, args, nil
}

func versionInfo() buildversion.Info {
	return buildversion.Resolve(buildversion.Info{
		Version: version,
		Commit:  commit,
		Date:    date,
	})
}

func runSelfUpdate(target string) error {
	info := versionInfo()
	result, err := buildversion.Update(context.Background(), buildversion.UpdateOptions{
		Repo:           "voocel/ainovel-cli",
		BinaryName:     "ainovel-cli",
		TargetVersion:  target,
		CurrentVersion: info.Version,
	})
	if err != nil {
		return err
	}
	if !result.Updated {
		fmt.Printf("ainovel-cli hiện đã là phiên bản mới nhất (%s)\n", result.Version)
		return nil
	}
	fmt.Printf("ainovel-cli đã được cập nhật lên phiên bản: %s\n", result.Version)
	fmt.Printf("Vị trí cài đặt: %s\n", result.Path)
	return nil
}

func loadPrompt(opts cliOptions) (string, error) {
	return loadPromptFrom(opts, os.Stdin)
}

func loadPromptFrom(opts cliOptions, stdin io.Reader) (string, error) {
	if opts.PromptFile == "" {
		return strings.TrimSpace(opts.Prompt), nil
	}

	if opts.PromptFile == "-" {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("đọc prompt thất bại: %w", err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	return startup.LoadPromptFile(opts.PromptFile)
}
