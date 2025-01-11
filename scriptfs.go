package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/gabriel-vasile/mimetype"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Directories map[string]DirConfig `yaml:"directories"`
}

type DirConfig struct {
	dirName     string
	Permissions uint32            `yaml:"permissions"`
	Files       map[string]File   `yaml:"files"`
	OnAction    map[string]Action `yaml:"onAction"`
	Defaults    *FileDefaults     `yaml:"defaults"`
}

type FileDefaults struct {
	Interpreter string        `yaml:"interpreter"`
	Timeout     time.Duration `yaml:"timeout"`
	Enqueue     bool          `yaml:"enqueue"`
}

type File struct {
	Permissions uint32                       `yaml:"permissions"`
	Receives    map[string]map[string]Action `yaml:"receives"`
	Type        string                       `yaml:"type"`
	ErrorMsg    string                       `yaml:"errorMsg"`
	Timeout     *time.Duration               `yaml:"timeout"`
	Enqueue     *bool                        `yaml:"enqueue"`
}

type Action struct {
	Script      string   `yaml:"script"`
	Interpreter string   `yaml:"interpreter"`
	Command     string   `yaml:"command"`
	Pipe        bool     `yaml:"pipe"`
	Uses        []string `yaml:"uses"`
}

type ScriptRoot struct {
	fs.Inode
	config *Config
}

type ScriptDir struct {
	fs.Inode
	config  DirConfig
	dirName string
	root    *ScriptRoot
}

type ScriptFile struct {
	fs.Inode
	mu        sync.Mutex
	config    File
	dirConfig DirConfig
	fileName  string
	cmd       *exec.Cmd
	queue     []string
	dataChan  chan []byte
	cmdCtx    context.Context
	cmdCancel context.CancelFunc
	cmdOnce   sync.Once
	cmdErr    error
	mimeType  *mimetype.MIME // Stores the detected MIME type
}

func (sf *ScriptFile) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	return &FileHandle{file: sf}, 0, 0
}

func (sf *ScriptFile) Setattr(ctx context.Context, attr *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	sf.mu.Lock()
	defer sf.mu.Unlock()

	if attr.Valid&fuse.FATTR_SIZE != 0 {
		// Resize content (not needed since we are streaming)
	}

	return 0
}

type FileHandle struct {
	file *ScriptFile
}

func (fh *FileHandle) Write(ctx context.Context, data []byte, off int64) (written uint32, errno syscall.Errno) {
	return fh.file.doWrite(ctx, data, off)
}

func (file *ScriptFile) doWrite(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	file.mu.Lock()
	defer file.mu.Unlock()

	log.Printf("Writing data: %d bytes to %s/%s", len(data), file.dirConfig.dirName, file.fileName)

	shouldEnqueue := determineEnqueue(file.config.Enqueue, file.dirConfig.Defaults)

	// Determine MIME type only once
	if file.mimeType == nil {
		file.mimeType = detectMimeType(data)
	}

	action, ok := determineAction(file.config.Receives, file.mimeType, "write")
	if !ok {
		log.Printf("No action defined for MIME type: %s in %s/%s", file.mimeType.String(), file.dirConfig.dirName, file.fileName)
		return 0, syscall.EIO
	}

	log.Printf("Executing action: %v for MIME type: %s in %s/%s", action, file.mimeType.String(), file.dirConfig.dirName, file.fileName)

	return handleWriteAction(file, data, shouldEnqueue, action)
}

func determineEnqueue(enqueue *bool, defaults *FileDefaults) bool {
	if enqueue != nil {
		return *enqueue
	}
	if defaults != nil {
		return defaults.Enqueue
	}
	return false
}

func determineAction(receives map[string]map[string]Action, mimeType *mimetype.MIME, actionType string) (Action, bool) {
	action, ok := receives[mimeType.String()][actionType]
	if !ok {
		log.Printf("No direct match for MIME type: %s", mimeType.String())
		for expectedMimetype := range receives {
			log.Printf("Checking expected MIME type: %s", expectedMimetype)
			if mimeType.Is(expectedMimetype) || reverse(filepath.Base(reverse(mimeType.String()))) == expectedMimetype {
				action, ok = receives[expectedMimetype][actionType]
				if ok {
					log.Printf("Matched expected MIME type: %s", expectedMimetype)
					break
				}
			}
		}
		if !ok {
			log.Printf("No match found for MIME type: %s, using fallback", mimeType.String())
			action, ok = receives["*"][actionType]
		}
	}
	log.Printf("Determined action: %v for MIME type: %s", action, mimeType.String())
	return action, ok
}

func handleWriteAction(file *ScriptFile, data []byte, shouldEnqueue bool, action Action) (uint32, syscall.Errno) {
	if shouldEnqueue && file.cmd != nil && file.cmd.Process != nil {
		file.queue = append(file.queue, string(data))
		return uint32(len(data)), 0
	}

	if file.cmd != nil && file.cmd.Process != nil {
		file.cmd.Process.Kill()
		file.cmd.Wait()
		file.cmd = nil
		file.cmdCancel()
		file.cmdOnce = sync.Once{} // Reset the once
	}

	execCtx := createExecutionContext(file.config.Timeout, file.dirConfig.Defaults)

	var err error
	var contentData []byte

	// Handle template content if needed
	if contains(action.Uses, "content") {
		contentData = data // Store the data for template use
	}

	if action.Command != "" {
		file.cmdOnce.Do(func() {
			file.dataChan = make(chan []byte, 1) // Add buffer of 1
			file.cmdCtx, file.cmdCancel = context.WithCancel(execCtx)
			go func() {
				defer close(file.dataChan)
				defer file.cmdCancel()
				_, file.cmdErr = executeCommand(file.cmdCtx, action.Command, file.dataChan, action.Pipe, contentData)
				if file.cmdErr != nil {
					log.Printf("Error executing command: %v in %s/%s", file.cmdErr, file.dirConfig.dirName, file.fileName)
				}
			}()
		})

		// Check for immediate command failure
		select {
		case <-file.cmdCtx.Done():
			log.Printf("Command failed to start or exited immediately in %s/%s", file.dirConfig.dirName, file.fileName)
			return 0, syscall.EIO
		default:
		}

		// Try to write with timeout
		writeCtx, cancel := context.WithTimeout(file.cmdCtx, 5*time.Second)
		defer cancel()

		select {
		case file.dataChan <- data:
			return uint32(len(data)), 0
		case <-writeCtx.Done():
			log.Printf("Write timeout or command exited in %s/%s", file.dirConfig.dirName, file.fileName)
			return 0, syscall.EIO
		}
	} else if action.Script != "" && action.Interpreter != "" {
		_, err = executeScript(execCtx, action.Interpreter, action.Script, data)
	}

	if err != nil {
		log.Printf("Error executing action: %v in %s/%s", err, file.dirConfig.dirName, file.fileName)
		return 0, syscall.EIO
	}

	return uint32(len(data)), 0
}

func createExecutionContext(timeout *time.Duration, defaults *FileDefaults) context.Context {
	execCtx := context.Background()
	if timeout != nil {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(execCtx, *timeout)
		defer cancel()
	} else if defaults != nil && defaults.Timeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(execCtx, defaults.Timeout)
		defer cancel()
	}
	return execCtx
}

func detectMimeType(data []byte) *mimetype.MIME {
	// Only pass the first 4096 bytes of data
	limitedData := data
	if len(data) > 4096 {
		limitedData = data[:4096]
	}
	mtype := mimetype.Detect(limitedData)
	log.Printf("Detected MIME type: %s", mtype.String())
	return mtype
}

func executeTemplate(tmpl string, data interface{}) (string, error) {
	t, err := template.New("script").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var result strings.Builder
	if err := t.Execute(&result, data); err != nil {
		return "", err
	}
	return result.String(), nil
}

func executeScript(ctx context.Context, interpreter, script string, data []byte) (string, error) {
	if interpreter == "" {
		return "", fmt.Errorf("interpreter not specified")
	}

	interpretedInterpreter, err := executeTemplate(interpreter, data)
	if err != nil {
		return "", err
	}

	interpretedScript, err := executeTemplate(script, data)
	if err != nil {
		return "", err
	}

	parts := strings.Fields(interpretedInterpreter)
	if len(parts) == 0 {
		return "", fmt.Errorf("interpreter format is invalid")
	}

	interpreterPath, err := exec.LookPath(parts[0])
	if err != nil || interpreterPath == "" {
		return "", fmt.Errorf("interpreter '%s' does not exist", interpreter)
	}

	cmd := exec.CommandContext(ctx, parts[0], append(parts[1:], interpretedScript)...)
	cmdOutput, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(cmdOutput)), nil
}

func executeCommand(ctx context.Context, command string, dataChan chan []byte, pipe bool, contentData []byte) (string, error) {
	if command == "" {
		return "", fmt.Errorf("command not specified")
	}

	// Apply template before executing the command if we have content data
	if len(contentData) > 0 {
		tmplData := map[string]interface{}{
			"Content": string(contentData),
		}
		var err error
		command, err = executeTemplate(command, tmplData)
		if err != nil {
			return "", fmt.Errorf("failed to execute template: %v", err)
		}
	}

	log.Printf("Executing command: %s", command)

	cmd := exec.CommandContext(ctx, "sh", "-c", command)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if pipe {
		pr, pw, err := os.Pipe()
		if err != nil {
			return "", fmt.Errorf("failed to create pipe: %v", err)
		}

		cmd.Stdin = pr

		// Start the command before starting the pipe goroutine
		if err := cmd.Start(); err != nil {
			pr.Close()
			pw.Close()
			return "", fmt.Errorf("command start failed: %v", err)
		}

		go func() {
			defer pw.Close()
			defer pr.Close()

			for data := range dataChan {
				if _, err := pw.Write(data); err != nil {
					log.Printf("Error writing to pipe: %v", err)
					return
				}
			}
		}()

		// Wait for command completion in a goroutine
		waitErr := make(chan error, 1)
		go func() {
			waitErr <- cmd.Wait()
		}()

		// Wait for either context cancellation or command completion
		select {
		case err := <-waitErr:
			if err != nil {
				log.Printf("Command failed: %v\nStderr: %s", err, stderr.String())
				return "", fmt.Errorf("command failed: %v", err)
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}
	} else {
		if err := cmd.Run(); err != nil {
			log.Printf("Command failed: %v\nStderr: %s", err, stderr.String())
			return "", fmt.Errorf("command failed: %v", err)
		}
	}

	return stdout.String(), nil
}

func (root *ScriptRoot) OnAdd(ctx context.Context) {
	for dirName, dirConfig := range root.config.Directories {
		dirConfig.dirName = dirName // Initialize dirName
		dir := &ScriptDir{
			config:  dirConfig,
			dirName: dirName,
			root:    root,
		}
		inode := root.NewPersistentInode(
			ctx,
			dir,
			fs.StableAttr{
				Mode: syscall.S_IFDIR | dirConfig.Permissions,
				Ino:  fs.StableAttr{}.Ino,
			},
		)
		root.AddChild(dirName, inode, true)
	}
}

func (dir *ScriptDir) OnAdd(ctx context.Context) {
	for fileName, fileConfig := range dir.config.Files {
		child := &ScriptFile{
			config:    fileConfig,
			dirConfig: dir.config,
			fileName:  fileName,
		}
		dir.NewPersistentInode(ctx, child, fs.StableAttr{Mode: fuse.S_IFREG | fileConfig.Permissions})
	}
}

func (dir *ScriptDir) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	entries := make([]fuse.DirEntry, 0, len(dir.config.Files))
	for name, file := range dir.config.Files {
		entries = append(entries, fuse.DirEntry{
			Mode: fuse.S_IFREG | file.Permissions,
			Name: name,
		})
	}
	return fs.NewListDirStream(entries), 0
}

func (dir *ScriptDir) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = fuse.S_IFDIR | dir.config.Permissions
	return 0
}

func (file *ScriptFile) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = fuse.S_IFREG | file.config.Permissions
	// out.Size = uint64(len(file.content)) // Not needed since we are streaming
	return 0
}

func (dir *ScriptDir) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if fileConfig, ok := dir.config.Files[name]; ok {
		child := &ScriptFile{
			config:    fileConfig,
			dirConfig: dir.config,
			fileName:  name,
		}
		return dir.NewPersistentInode(ctx, child, fs.StableAttr{Mode: fuse.S_IFREG | fileConfig.Permissions}), 0
	}
	return nil, syscall.ENOENT
}

func (file *ScriptFile) Read(ctx context.Context, f fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	file.mu.Lock()
	defer file.mu.Unlock()

	// Reading is not needed since we are streaming
	return nil, 0
}

func (file *ScriptFile) Write(ctx context.Context, f fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	file.mu.Lock()
	defer file.mu.Unlock()

	log.Printf("Writing data: %d bytes", len(data))

	shouldEnqueue := determineEnqueue(file.config.Enqueue, file.dirConfig.Defaults)

	// Determine MIME type only once
	if file.mimeType == nil {
		file.mimeType = detectMimeType(data)
	}

	action, ok := determineAction(file.config.Receives, file.mimeType, "write")
	if !ok {
		log.Printf("No action defined for MIME type: %s", file.mimeType.String())
		return 0, syscall.EIO
	}

	return handleWriteAction(file, data, shouldEnqueue, action)
}

func main() {
	configPath := flag.String("config", "scripts.yaml", "Path to config file")
	mountPoint := flag.String("mount", "/tmp/scriptfs", "Mount point")
	debugFlag := flag.Bool("debug", false, "Enable FUSE's debug output")
	flag.Parse()

	configData, err := os.ReadFile(*configPath)
	if err != nil {
		log.Fatalf("Failed to read config: %v", err)
	}

	var config Config
	if err := yaml.Unmarshal(configData, &config); err != nil {
		log.Fatalf("Failed to parse config: %v", err)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	root := &ScriptRoot{config: &config}

	server, err := fs.Mount(*mountPoint, root, &fs.Options{
		MountOptions: fuse.MountOptions{
			Debug: *debugFlag,
		},
	})
	if err != nil {
		log.Fatalf("Mount failed: %v", err)
	}

	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		<-sigChan
		log.Println("Received signal, unmounting...")
		cancel()
		if err := server.Unmount(); err != nil {
			log.Printf("Unmount failed: %v", err)
		}
	}()

	server.Wait()
}

func reverse(s string) string {
	runes := []rune(s)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}
