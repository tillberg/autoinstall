# autoinstall

Continuously go-installs all packages in GOPATH.

- Uses filesystem-monitoring (via [howeyc/fsnotify][howeyc/fsnotify])
- Parses package imports and recursively kicks off builds whenever dependencies are updated

### Quick Start

You must already have Go installed and have GOPATH set in your environment. Then just run:

```sh
$ go install github.com/tillberg/autoinstall
$ $GOPATH/bin/autoinstall
```

Use `--help` to see additional options.

![Animated Gif demonstrating autoinstall](https://www.tillberg.us/c/c99aebe723954893cb20290679facbe294ca800ae0c6e6b08da84c2d5ef89f5c/autoinstall.gif)

### License

ISC License

[howeyc/fsnotify]: https://github.com/howeyc/fsnotify
