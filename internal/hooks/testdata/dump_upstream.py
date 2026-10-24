import json, shlex, sys, logging
sys.path.insert(0, ".")
logging.disable(logging.CRITICAL)
from agent import shell_hooks as sh

def split_win(line):
    out = []
    for tok in shlex.split(line, posix=False):
        if len(tok) >= 2 and tok[0] == tok[-1] and tok[0] in ("'", '"'):
            tok = tok[1:-1]
        out.append(tok)
    return out

def safe(f, x):
    try: return f(x)
    except ValueError: return None

lines = ['a b  c', 'echo "hi there" x', "it's", "'a b'c", '"a b"c', r'C:\Users\me\x.sh --f', r'a\ b', r'"a\"b"', r"'a\b'", 'x "unterminated', 'end' + chr(92),'~/hooks/x.sh', r'"C:\Program Files\x.exe" arg', 'a"b c"d']
split = [{"line": l, "posix": safe(shlex.split, l), "windows": safe(split_win, l)} for l in lines]

def spec(event="pre_tool_call", fc=False, cmd="hook.sh"):
    return sh.ShellHookSpec(event=event, command=cmd, timeout=60, fail_closed=fc)
def r(code=0, out="", err="", timed=False, error=None):
    return {"returncode": code, "stdout": out, "stderr": err, "timed_out": timed, "elapsed_seconds": 0.1, "error": error}
cases = []
for ev in ["pre_tool_call", "post_tool_call"]:
    for fc in [False, True]:
        for name, res in [
            ("ok_empty", r()), ("exit2_plain", r(2)), ("exit2_stderr", r(2, err="  nope \n")),
            ("exit2_json", r(2, out='{"decision":"block","reason":"why"}')), ("exit2_json_other", r(2, out='{"context":"c"}', err="e")),
            ("exit1", r(1)), ("exit1_block", r(1, out='{"action":"block","message":"m"}')),
            ("garbage", r(0, out="Traceback")), ("garbage_exit1", r(1, out="Traceback")),
            ("list", r(0, out="[1]")), ("timeout", r(None, timed=True)), ("error", r(None, error="command not found")),
            ("block_action", r(0, out='{"action":"block","message":"m","reason":"r"}')),
            ("block_empty_msg", r(0, out='{"action":"block","message":"","reason":"r"}')),
            ("block_nonstr", r(0, out='{"action":"block","message":5}')),
            ("block_none", r(0, out='{"action":"block"}')),
            ("decision_block", r(0, out='{"decision":"block","reason":"r","message":"m"}')),
            ("modify", r(0, out='{"action":"modify","args":{"a":1}}')),
            ("modify_cc", r(0, out='{"decision":"modify","tool_input":{"b":2}}')),
            ("modify_bad", r(0, out='{"action":"modify","args":[1]}')),
            ("approve", r(0, out='{"action":"approve","message":" m ","rule_key":" k "}')),
            ("approve_bare", r(0, out='{"action":"approve"}')),
            ("context", r(0, out='{"context":"hello"}')), ("context_blank", r(0, out='{"context":"  "}')),
            ("long_stderr", r(2, err="x"*500)),
        ]:
            cases.append({"event": ev, "fail_closed": fc, "name": name, "result": res, "want": sh._evaluate_result(spec(ev, fc), res)})
print(json.dumps({"split": split, "evaluate": cases}, indent=1, ensure_ascii=False))
