package approval

import (
	"path"
	"regexp"
	"strings"
)

// Structural execution-flag detection, a subset of upstream
// _execution_flag_findings: interpreters run with an inline script, shells
// run with -c (whose payload is scanned again), and read tools that take a
// program to execute.

var interpreterNames = []struct {
	family string
	re     *regexp.Regexp
}{
	{"python", regexp.MustCompile(`^(?:py(?:\.exe)?|python[23]?(?:\.\d+)*(?:\.exe)?)$`)},
	{"node", regexp.MustCompile(`^node(?:js)?(?:\.exe)?$`)},
	{"perl", regexp.MustCompile(`^perl[0-9]*(?:\.\d+)*(?:\.exe)?$`)},
	{"ruby", regexp.MustCompile(`^ruby[0-9.]*(?:\.exe)?$`)},
	{"php", regexp.MustCompile(`^php(?:\.exe)?$`)},
	{"powershell", regexp.MustCompile(`^(?:powershell|pwsh)(?:\.exe)?$`)},
	{"bun", regexp.MustCompile(`^bun(?:\.exe)?$`)},
	{"deno", regexp.MustCompile(`^deno(?:\.exe)?$`)},
}

var interpreterExecFlags = map[string][]string{
	"python": {"-c"}, "node": {"-e", "--eval", "-p", "--print"}, "perl": {"-e", "--eval"}, "ruby": {"-e"},
	"php": {"-r"}, "powershell": {"-command", "-c", "-file", "-f"},
	"bun": {"-e", "--eval"}, "deno": {"eval", "-e", "--eval"},
}

var interpreterWithArg = map[string][]string{
	"python": {"-W", "-X", "--check-hash-based-pycs"},
	"node": {"-C", "--conditions", "--cpu-prof-dir", "--diagnostic-dir", "--icu-data-dir", "--import", "--loader",
		"--openssl-config", "--require", "--title"},
	"perl": {"-0", "-F", "-I", "-M", "-m", "-x"}, "ruby": {"-C", "-E", "-F", "-I", "-K", "-r"},
	"php": {"-c", "-d", "-z"},
	"powershell": {"-configurationname", "-custompipename", "-executionpolicy", "-inputformat", "-outputformat",
		"-settingsfile", "-version", "-windowstyle", "-workingdirectory"},
	"bun": {"--config", "--cwd", "--env-file", "--preload", "--require"}, "deno": {"-L", "--log-level"},
}

var readToolExecFlags = map[string][]string{
	"sort": {"--compress-program"}, "rg": {"--pre", "--hostname-bin"}, "ag": {"--pager"},
	"man": {"--pager", "--html", "-P", "-H"},
}

var shellNames = map[string]bool{"bash": true, "sh": true, "zsh": true, "ksh": true, "dash": true}

var (
	assignRE  = regexp.MustCompile(`^[A-Za-z_]\w*=`)
	wrapperRE = regexp.MustCompile(`^(?:exec|nohup|setsid|time|command|builtin)$`)
)

type finding struct {
	description string
	payload     string
}

func interpreterFamily(name string) string {
	for _, n := range interpreterNames {
		if n.re.MatchString(name) {
			return n.family
		}
	}
	return ""
}

// execFindings scans every command segment of a normalized command.
func execFindings(cmd string) []finding {
	var out []finding
	for _, words := range segments(cmd) {
		words = stripWrappers(words)
		if len(words) == 0 {
			continue
		}
		name := strings.ToLower(path.Base(strings.ReplaceAll(words[0], `\`, "/")))
		args := words[1:]
		if family := interpreterFamily(name); family != "" {
			if hasExecFlag(family, args) {
				out = append(out, finding{description: "script execution via -e/-c flag"})
			} else if anyPrefix(args, "<<") {
				out = append(out, finding{description: "script execution via heredoc"})
			}
			continue
		}
		if shellNames[name] {
			if payload, ok := shellPayload(args); ok {
				out = append(out, finding{description: "shell command via -c/-lc flag", payload: payload})
			}
		}
		for _, flag := range readToolExecFlags[name] {
			for i, a := range args {
				if a == flag || strings.HasPrefix(a, flag+"=") {
					payload := strings.TrimPrefix(strings.TrimPrefix(a, flag), "=")
					if payload == "" && i+1 < len(args) {
						payload = args[i+1]
					}
					out = append(out, finding{description: "arbitrary program execution via " + name + " " + flag, payload: payload})
				}
			}
		}
	}
	return out
}

func anyPrefix(args []string, p string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, p) {
			return true
		}
	}
	return false
}

// stripWrappers drops sudo (and its flags), env, VAR=value prefixes and
// exec/nohup-style wrappers in front of the real program.
func stripWrappers(w []string) []string {
	for len(w) > 0 {
		switch {
		case w[0] == "sudo":
			w = w[1:]
			for len(w) > 0 && strings.HasPrefix(w[0], "-") {
				w = w[1:]
			}
		case w[0] == "env":
			w = w[1:]
			for len(w) > 0 && (assignRE.MatchString(w[0]) || strings.HasPrefix(w[0], "-")) {
				w = w[1:]
			}
		case assignRE.MatchString(w[0]) || wrapperRE.MatchString(w[0]):
			w = w[1:]
		default:
			return w
		}
	}
	return w
}

func hasExecFlag(family string, args []string) bool {
	in := func(list []string, a string) bool {
		for _, x := range list {
			if a == x || (family == "powershell" && strings.EqualFold(a, x)) {
				return true
			}
		}
		return false
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if in(interpreterExecFlags[family], a) || strings.HasPrefix(a, "--eval=") {
			return true
		}
		if family == "python" && strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a[1:], "c") &&
			!in(interpreterWithArg[family], a[:2]) {
			return true
		}
		if !strings.HasPrefix(a, "-") {
			return false
		}
		if in(interpreterWithArg[family], a) {
			i++
		}
	}
	return false
}

// shellPayload returns the -c argument of a shell invocation.
func shellPayload(args []string) (string, bool) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") || a == "-" || a == "--" {
			return "", false
		}
		if !strings.HasPrefix(a, "--") && strings.Contains(a[1:], "c") {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", true
		}
	}
	return "", false
}

// segments splits a command into simple commands and their unquoted words.
func segments(s string) [][]string {
	var (
		out   [][]string
		words []string
		cur   strings.Builder
		inTok bool
	)
	endWord := func() {
		if inTok {
			words = append(words, cur.String())
			cur.Reset()
			inTok = false
		}
	}
	endSeg := func() {
		endWord()
		if len(words) > 0 {
			out = append(out, words)
			words = nil
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'' || c == '"':
			j := strings.IndexByte(s[i+1:], c)
			if j < 0 {
				j = len(s) - i - 1
			}
			cur.WriteString(s[i+1 : i+1+j])
			inTok = true
			i += j + 1
		case strings.IndexByte(";|&\n()`{}", c) >= 0:
			endSeg()
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			endSeg()
			i++
		case isSpace(c):
			endWord()
		default:
			cur.WriteByte(c)
			inTok = true
		}
	}
	endSeg()
	return out
}
