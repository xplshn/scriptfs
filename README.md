# scriptfs

**scriptfs** lets you turn your filesystem into a scriptable resource using a simple `scripts.yaml` configuration file. Define custom actions for file operations and handle different MIME types with ease.

## Features

- **Custom Actions**: Define what happens when you write to a file.
- **MIME Type Detection**: Automatically detect and handle different MIME types of contents sent to a file.
- **Command Execution**: Run commands or scripts based on file operations and mimetype
- **Templating**: Use Go text/templating with sprig v3 functions for dynamic content.

## Installation

1. Make sure you have Go installed.
2. Clone the repository and build the project:

```sh
git clone --depth=1 https://github.com/xplshn/scriptfs
cd scriptfs
go build -o scriptfs
```

## Configuration

Create a `scripts.yaml` file to define your filesystem behavior. Here's an example:

```yaml
directories:
  audio:
    permissions: 0755
    files:
      play:
        permissions: 0644
        receives:
          "audio":
            write:
              command: "ffmpeg -ar 48000 -ac 2 -f wav pipe:1 -i - | aplay"
              pipe: true
          "audio/x-wav":
            write:
              command: "aplay"
              pipe: true
          "*": # In case the data sent to the audio/play file is of an unknown or unhandled type
            write:
              uses:
                - "template"
                - "content"
              command: "echo 'Unknown data received; {{.Content}}'"
  anotherDirectoryDefinition:
    permissions: 0755
    files:
      anotherFileDefinition:
        permissions: 0644
        receives:
          "image":
            write:
              command: "swayimg -"
              pipe: true
defaults:
  interpreter: "sh -c"
  timeout: "10s"
  enqueue: true
```

## Usage

Run **scriptfs**:

```sh
./scriptfs --config path/to/scripts.yaml --mount /path/to/mountpoint
```

- `--config`: Path to your `scripts.yaml` file.
- `--mount`: Where you want to mount the filesystem.
- `--debug`: (Optional) Enable debug output.

## Show & Tell
![image](https://github.com/user-attachments/assets/fa118c25-b842-4b1a-b994-8de9a29e1bc3)

## License

**scriptfs** is licensed under the following licenses: Choose whichever fits your needs best:
- ISC (Pre-2007 ISC License, the one the OpenBSD project uses)
- MIT-0 (MIT ZERO attribution)
- Unlicense (The Unlicense)

## Contributing

Feel free to open an issue or submit a pull request to this repo.

## Acknowledgments

- **Go-Fuse**: For the FUSE library.
- **mimetype**: For MIME type detection.
- **sprig**: For additional template functions.

###### TODO; Provide examples for actions of type "read", as well as add support for actions for type "list" and "execute", etc.
###### TODO; Figure out what to do with errorMsg (YAML key that lets you specify custom error messages) -> Either remove such functionality or come up with a __viable__ alternative.

## Contact

For questions or issues, open an issue on this repo
