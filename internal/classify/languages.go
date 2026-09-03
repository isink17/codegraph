package classify

// Language knowledge sets (P8).
//
// Every set here is package-level and read-only: maps are allocated once at
// init and only ever read, so classifying an edge copies nothing. Callers must
// not mutate them.
//
// The sets are curated, not generated. Each one answers a narrow question --
// "is this identifier predeclared by the language?", "is this import path
// reserved by the platform?" -- and admits only names where the answer is
// unarguable. A name is left out when including it would require an assumption
// about how the project is built, packaged, or deployed.

// builtinsByLanguage holds callable language intrinsics that need no import and
// no project definition. Only languages whose parser actually emits such calls
// as edges appear here; the rest classify builtins as `unknown`.
//
// A language's absence from this map means "no builtin rule", not "no builtins".
var builtinsByLanguage = map[string]map[string]bool{
	"go":         goBuiltins,
	"python":     pythonBuiltins,
	"typescript": typescriptBuiltins,
}

// typescriptBuiltins contains ECMAScript globals whose identity is guaranteed
// by the language/runtime specification. Runtime-specific globals such as
// console, Buffer, and process are intentionally absent: a .ts/.js file does
// not prove that it targets Node or a browser.
var typescriptBuiltins = map[string]bool{
	"Array": true, "BigInt": true, "Boolean": true, "Date": true,
	"Error": true, "EvalError": true, "FinalizationRegistry": true,
	"Function": true, "Map": true, "Number": true, "Object": true,
	"Promise": true, "Proxy": true, "RangeError": true, "ReferenceError": true,
	"RegExp": true, "Set": true, "String": true, "Symbol": true,
	"SyntaxError": true, "TypeError": true, "URIError": true, "WeakMap": true,
	"WeakRef": true, "WeakSet": true, "JSON": true, "Math": true,
	"Reflect": true, "Intl": true,
	// Directly callable ECMAScript globals.
	"decodeURI": true, "decodeURIComponent": true, "encodeURI": true,
	"encodeURIComponent": true, "eval": true, "isFinite": true, "isNaN": true,
	"parseFloat": true, "parseInt": true,
}

// goBuiltins is the complete predeclared identifier set from the Go spec's
// `builtin` package -- functions plus predeclared types, both of which appear as
// call expressions (`append(s, x)`, `int64(n)`) and are therefore emitted as
// call edges by both Go adapters.
//
// Predeclared constants (`true`, `false`, `iota`) and `nil` are excluded: they
// are never called, so they cannot be a call edge's destination.
//
// Shadowing is real -- Go permits `func len(...)` at package scope -- which is
// exactly why the builtin rule additionally requires that no same-language
// project symbol claims the name (Evidence.ProjectDefinesTarget).
var goBuiltins = map[string]bool{
	// Functions.
	"append": true, "cap": true, "clear": true, "close": true, "complex": true,
	"copy": true, "delete": true, "imag": true, "len": true, "make": true,
	"max": true, "min": true, "new": true, "panic": true, "print": true,
	"println": true, "real": true, "recover": true,
	// Predeclared types, callable as conversions.
	"any": true, "bool": true, "byte": true, "comparable": true,
	"complex64": true, "complex128": true, "error": true, "float32": true,
	"float64": true, "int": true, "int8": true, "int16": true, "int32": true,
	"int64": true, "rune": true, "string": true, "uint": true, "uint8": true,
	"uint16": true, "uint32": true, "uint64": true, "uintptr": true,
}

// pythonBuiltins is the callable contents of CPython's `builtins` module:
// functions, types, and the built-in exception hierarchy. All are resolvable
// with no import statement anywhere in the file, which is what makes them
// classifiable from a bare name alone.
//
// Non-callable members (`None`, `True`, `Ellipsis`, `__debug__`) are excluded --
// they cannot be a call target. `print` is present for completeness even though
// both Python adapters filter it out of their edge output as a keyword.
var pythonBuiltins = map[string]bool{
	// Functions and types.
	"abs": true, "aiter": true, "all": true, "anext": true, "any": true,
	"ascii": true, "bin": true, "bool": true, "breakpoint": true,
	"bytearray": true, "bytes": true, "callable": true, "chr": true,
	"classmethod": true, "compile": true, "complex": true, "delattr": true,
	"dict": true, "dir": true, "divmod": true, "enumerate": true, "eval": true,
	"exec": true, "filter": true, "float": true, "format": true,
	"frozenset": true, "getattr": true, "globals": true, "hasattr": true,
	"hash": true, "help": true, "hex": true, "id": true, "input": true,
	"int": true, "isinstance": true, "issubclass": true, "iter": true,
	"len": true, "list": true, "locals": true, "map": true, "max": true,
	"memoryview": true, "min": true, "next": true, "object": true, "oct": true,
	"open": true, "ord": true, "pow": true, "print": true, "property": true,
	"range": true, "repr": true, "reversed": true, "round": true, "set": true,
	"setattr": true, "slice": true, "sorted": true, "staticmethod": true,
	"str": true, "sum": true, "super": true, "tuple": true, "type": true,
	"vars": true, "zip": true, "__import__": true,
	// Exceptions and warnings.
	"ArithmeticError": true, "AssertionError": true, "AttributeError": true,
	"BaseException": true, "BaseExceptionGroup": true, "BlockingIOError": true,
	"BrokenPipeError": true, "BufferError": true, "BytesWarning": true,
	"ChildProcessError": true, "ConnectionAbortedError": true,
	"ConnectionError": true, "ConnectionRefusedError": true,
	"ConnectionResetError": true, "DeprecationWarning": true, "EOFError": true,
	"EncodingWarning": true, "EnvironmentError": true, "Exception": true,
	"ExceptionGroup": true, "FileExistsError": true, "FileNotFoundError": true,
	"FloatingPointError": true, "FutureWarning": true, "GeneratorExit": true,
	"IOError": true, "ImportError": true, "ImportWarning": true,
	"IndentationError": true, "IndexError": true, "InterruptedError": true,
	"IsADirectoryError": true, "KeyError": true, "KeyboardInterrupt": true,
	"LookupError": true, "MemoryError": true, "ModuleNotFoundError": true,
	"NameError": true, "NotADirectoryError": true, "NotImplementedError": true,
	"OSError": true, "OverflowError": true, "PendingDeprecationWarning": true,
	"PermissionError": true, "ProcessLookupError": true, "RecursionError": true,
	"ReferenceError": true, "ResourceWarning": true, "RuntimeError": true,
	"RuntimeWarning": true, "StopAsyncIteration": true, "StopIteration": true,
	"SyntaxError": true, "SyntaxWarning": true, "SystemError": true,
	"SystemExit": true, "TabError": true, "TimeoutError": true,
	"TypeError": true, "UnboundLocalError": true, "UnicodeDecodeError": true,
	"UnicodeEncodeError": true, "UnicodeError": true,
	"UnicodeTranslateError": true, "UnicodeWarning": true, "UserWarning": true,
	"ValueError": true, "Warning": true, "ZeroDivisionError": true,
}

// goStdlibRoots is the set of importable top-level standard-library package
// paths (`fmt`, `net` covering `net/http`, ...).
//
// Go reserves import paths whose first segment contains no dot for the standard
// library, but that rule alone would also accept a locally replaced module
// named `myapp`, so classifyGo requires membership here too. Deliberately
// excluded: `internal` and `cmd` (not importable by user code), and `builtin`
// (documentation only).
//
// Residual gap: a module whose path is literally a stdlib root (`module net`)
// would have its own packages read as stdlib. Such a module cannot be published
// or fetched, so this is accepted rather than guarded.
var goStdlibRoots = map[string]bool{
	"archive": true, "bufio": true, "bytes": true, "cmp": true,
	"compress": true, "container": true, "context": true, "crypto": true,
	"database": true, "debug": true, "embed": true, "encoding": true,
	"errors": true, "expvar": true, "flag": true, "fmt": true, "go": true,
	"hash": true, "html": true, "image": true, "index": true, "io": true,
	"iter": true, "log": true, "maps": true, "math": true, "mime": true,
	"net": true, "os": true, "path": true, "plugin": true, "reflect": true,
	"regexp": true, "runtime": true, "slices": true, "sort": true,
	"strconv": true, "strings": true, "structs": true, "sync": true,
	"syscall": true, "testing": true, "text": true, "time": true,
	"unicode": true, "unique": true, "unsafe": true, "weak": true,
}

// pythonStdlibModules is the set of top-level modules and packages shipped with
// CPython. Platform-specific ones are included because the module is standard
// wherever it exists; whether the target platform has it is a runtime question,
// not an origin question.
//
// Residual gap: Python's `sys.path[0]` lets a project file named `logging.py`
// shadow the stdlib module of the same name. Such a call almost always resolves
// to the project symbol instead (making it `project`, never reaching this set),
// so the case is documented rather than guarded with an extra query.
var pythonStdlibModules = map[string]bool{
	"abc": true, "argparse": true, "array": true, "ast": true,
	"asyncio": true, "atexit": true, "base64": true, "bdb": true,
	"binascii": true, "bisect": true, "builtins": true, "bz2": true,
	"calendar": true, "cmath": true, "cmd": true, "code": true, "codecs": true,
	"collections": true, "colorsys": true, "compileall": true,
	"concurrent": true, "configparser": true, "contextlib": true,
	"contextvars": true, "copy": true, "copyreg": true, "csv": true,
	"ctypes": true, "curses": true, "dataclasses": true, "datetime": true,
	"dbm": true, "decimal": true, "difflib": true, "dis": true,
	"doctest": true, "email": true, "encodings": true, "ensurepip": true,
	"enum": true, "errno": true, "faulthandler": true, "fcntl": true,
	"filecmp": true, "fileinput": true, "fnmatch": true, "fractions": true,
	"ftplib": true, "functools": true, "gc": true, "getopt": true,
	"getpass": true, "gettext": true, "glob": true, "graphlib": true,
	"grp": true, "gzip": true, "hashlib": true, "heapq": true, "hmac": true,
	"html": true, "http": true, "idlelib": true, "imaplib": true,
	"importlib": true, "inspect": true, "io": true, "ipaddress": true,
	"itertools": true, "json": true, "keyword": true, "linecache": true,
	"locale": true, "logging": true, "lzma": true, "mailbox": true,
	"marshal": true, "math": true, "mimetypes": true, "mmap": true,
	"modulefinder": true, "multiprocessing": true, "netrc": true,
	"numbers": true, "operator": true, "optparse": true, "os": true,
	"pathlib": true, "pdb": true, "pickle": true, "pickletools": true,
	"pkgutil": true, "platform": true, "plistlib": true, "poplib": true,
	"posix": true, "pprint": true, "profile": true, "pstats": true,
	"pty": true, "pwd": true, "py_compile": true, "pyclbr": true,
	"pydoc": true, "queue": true, "quopri": true, "random": true, "re": true,
	"readline": true, "reprlib": true, "resource": true, "runpy": true,
	"sched": true, "secrets": true, "select": true, "selectors": true,
	"shelve": true, "shlex": true, "shutil": true, "signal": true,
	"site": true, "smtplib": true, "socket": true, "socketserver": true,
	"sqlite3": true, "ssl": true, "stat": true, "statistics": true,
	"string": true, "stringprep": true, "struct": true, "subprocess": true,
	"symtable": true, "sys": true, "sysconfig": true, "syslog": true,
	"tabnanny": true, "tarfile": true, "tempfile": true, "termios": true,
	"textwrap": true, "threading": true, "time": true, "timeit": true,
	"tkinter": true, "token": true, "tokenize": true, "tomllib": true,
	"trace": true, "traceback": true, "tracemalloc": true, "tty": true,
	"turtle": true, "types": true, "typing": true, "unicodedata": true,
	"unittest": true, "urllib": true, "uuid": true, "venv": true,
	"warnings": true, "wave": true, "weakref": true, "webbrowser": true,
	"wsgiref": true, "xml": true, "xmlrpc": true, "zipapp": true,
	"zipfile": true, "zipimport": true, "zlib": true, "zoneinfo": true,
}

// rustStdlibCrates is the standard distribution's crate set. `test` and
// `proc_macro` ship with the toolchain too but are excluded: `test` is unstable
// and easily shadowed by a project module of that name.
var rustStdlibCrates = map[string]bool{
	"std": true, "core": true, "alloc": true,
}

// nodeCoreModules is the set of Node.js built-in module names. A bare specifier
// matching one of these resolves to the runtime, never to `node_modules`, which
// is what separates it from classifyTypeScript's `external` branch. The
// `node:`-prefixed form needs no list and is handled by prefix.
//
// Residual gap: a `.ts`/`.js` file may target a browser, where these names are
// bundler shims rather than a platform library. The specifier alone cannot say
// which, and `stdlib` is the closer of the two available answers for a
// non-project runtime module.
var nodeCoreModules = map[string]bool{
	"assert": true, "async_hooks": true, "buffer": true, "child_process": true,
	"cluster": true, "console": true, "constants": true, "crypto": true,
	"dgram": true, "diagnostics_channel": true, "dns": true, "domain": true,
	"events": true, "fs": true, "http": true, "http2": true, "https": true,
	"inspector": true, "module": true, "net": true, "os": true, "path": true,
	"perf_hooks": true, "process": true, "punycode": true, "querystring": true,
	"readline": true, "repl": true, "stream": true, "string_decoder": true,
	"sys": true, "timers": true, "tls": true, "trace_events": true,
	"tty": true, "url": true, "util": true, "v8": true, "vm": true,
	"wasi": true, "worker_threads": true, "zlib": true,
}

// canonicalLanguages is the persisted language vocabulary used by the
// default parser registries. Capabilities() exposes it as a deterministic
// coverage table; internal/cli tests compare that table with the live registry.
var canonicalLanguages = []string{
	"cpp", "csharp", "go", "java", "kotlin", "php", "python", "ruby",
	"rust", "swift", "typescript",
}

// shadowingGap documents the one asymmetry in how project evidence is used.
//
// Only the builtin rule consults Evidence.ProjectDefinesTarget, because a
// builtin claim rests entirely on a bare name and a same-language project
// definition of that name directly contradicts it. The stdlib and external rules
// rest on an import path instead, and the corresponding contradiction would be a
// project *module* shadowing an imported one (a `json.py` beside `import json`,
// a project `utils` package beside a bare `utils` specifier). CodeGraph indexes
// no module list, so proving that would mean inferring module identity from file
// paths -- a new heuristic, which is what P8 exists to avoid.
//
// The residual false positive is narrow: the shadowing module must exist, the
// call must have gone unresolved anyway (normally it resolves and reports
// `project`), and the shadowed name must be a stdlib module or a bare specifier.
// The obvious wider guard -- abstaining whenever the project defines the called
// method name -- was rejected: any project function named `get` would erase
// `requests.get`, trading a rare wrong answer for a common missing one.
const shadowingGap = "stdlib and external do not abstain on project modules that shadow an imported name; " +
	"only builtin consults project-symbol evidence"

// conservativeGaps records what P8 deliberately refuses to classify, and why.
// For a language with no entry in importRules the gap is the whole language: no
// rule exists, so every edge is `unknown`. For a language that does have rules,
// the entry names the one dimension those rules still cannot prove.
//
// It is documentation with a test attached (see classify_test.go), so coverage
// cannot be widened without editing this table and stating the new evidence.
// The entries are not TODOs: each is a case where the missing evidence is a
// package manifest, an alias, or an imported-symbol list that CodeGraph does not
// persist at all.
var conservativeGaps = map[string]string{
	"cpp": "the `<vector>` vs `\"local.h\"` distinction is destroyed at parse " +
		"time (cpp_adapter.go trims both delimiter sets), so a system header " +
		"and a project header are the same string",
	"csharp": "`using` imports a namespace, not a type, so the identifier at " +
		"the call site never appears in the import path and cannot be linked",
	"kotlin": "stdlib members (`println`) are auto-imported, so the calls that " +
		"would be classifiable carry no import evidence at all",
	"php": "`use App\\Models\\User` and a vendor namespace are the same shape; " +
		"no composer autoload map is indexed",
	"ruby": "the `require` vs `require_relative` distinction is not persisted, " +
		"so a gem and a project file are the same bare string",
	"swift": "`import Foundation` and `import MyFramework` are the same " +
		"single-identifier form; no SDK or package manifest is indexed",
	"go": "external is unprovable: a dependency path and this repository's own " +
		"module path are both domain-form, and go.mod is never read",
	"python": "external is unprovable (no project package list); `from x import y` " +
		"drops `y`, so symbol imports cannot be linked; and the non-cgo adapter " +
		"drops receivers, so a bare builtin name is only claimed when the recorded " +
		"call site does not show a member access (nameUsedAsMember)",
	"java": "external is unprovable: a first-party `com.acme` and a " +
		"third-party `com.google` are indistinguishable without build metadata",
	"rust": "external is unprovable: since the 2018 uniform-path rules a bare " +
		"first segment may name an external crate or a crate-root module; and a " +
		"`std::`/`core::`/`alloc::` prefix in dst_name is trusted without a " +
		"project-shadowing check, so a crate with its own `mod core` is misread",
	"typescript": "named imports (`import {map} from \"lodash\"`) do not " +
		"persist the local binding, and a tsconfig `paths` alias can make a " +
		"bare specifier project-local",
}
