"""所有 workflow run 的 bash -n、引用/CLI 参数与 Python AST 静态检查；不执行 native 命令。"""
import argparse
import ast
import hashlib
import json
from pathlib import Path
import re
import shlex
import subprocess
import sys


def run_blocks(text):
    lines, result, index = text.splitlines(), [], 0
    while index < len(lines):
        match = re.match(r'^(\s*)(?:-\s+)?run:\s*(.*)$', lines[index])
        if not match:
            index += 1
            continue
        start = index
        raw = match[2]
        if raw in ('|', '|-', '|+', '>', '>-', '>+'):
            base_indent = len(match[1]) + (2 if lines[index].lstrip().startswith('- ') else 0)
            index += 1
            body = []
            while index < len(lines) and (not lines[index].strip() or
                    len(lines[index]) - len(lines[index].lstrip()) > base_indent):
                body.append(lines[index])
                index += 1
            indent = min((len(line) - len(line.lstrip()) for line in body if line.strip()), default=base_indent + 2)
            body = [line[indent:] if line.strip() else '' for line in body]
            script = ('\n' if raw.startswith('|') else ' ').join(body)
        else:
            index += 1
            if raw.startswith("'") and raw.endswith("'"):
                script = raw[1:-1].replace("''", "'")
            elif raw.startswith('"') and raw.endswith('"'):
                script = json.loads(raw)
            else:
                script = raw
        result.append((start + 1, re.sub(r'\$\{\{.*?\}\}', 'ci_expression_fixture', script) + '\n'))
    return result


def references(root, text):
    # 只匹配仓库输入；生成制品的 shell 变量路径不属于源码引用。
    values = set(re.findall(r'(?:deploy/[A-Za-z0-9_./-]+\.sh|tools/ci/v0222_[A-Za-z0-9_]+\.py|'
        r'\.github/release-tools/[A-Za-z0-9_./-]+|backend/(?:go\.(?:mod|sum)|cmd/server/VERSION)|'
        r'frontend/(?:pnpm-lock\.yaml|package\.json)|(?<=file: )\./Dockerfile)', text))
    for name in values:
        if not (root / name).is_file():
            raise ValueError('workflow input missing: ' + name)
    return sorted(values)


def helper_cli(root, script):
    flattened = script.replace('\\\n', ' ')
    result = []
    for match in re.finditer(r'(?:python3|python)\s+(?:-I\s+)?(?:-B\s+)?((?:\.\./)?tools/ci/v0222_\w+\.py)\s+([^\n]+)', flattened):
        helper = root / match[1].removeprefix('../')
        command_tail = re.sub(r'\)"\s*$', '', match[2])
        tokens = shlex.split(command_tail)
        if tokens and tokens[-1].endswith(')'):
            tokens[-1] = tokens[-1].rstrip(')')
        arguments, skip = [], False
        for index, token in enumerate(tokens):
            if token == '--':
                arguments += tokens[index:]
                break
            if skip:
                skip = False
                continue
            if token.startswith('--'):
                arguments.append(token)
                if token != '--execute-isolated-ci-probe':
                    if index + 1 >= len(tokens):
                        raise ValueError('missing CLI argument value')
                    arguments.append(tokens[index + 1] if token == '--mode' else
                                     ('1' if token in ('--exit-code', '--asset-count') else 'fixture'))
                    skip = True
            elif not arguments:
                arguments.append(token)
        # 只运行 argparse：从 AST 提取 main 内的 parser 构造，去掉 parse_args 后的运行代码。
        tree = ast.parse(helper.read_text(encoding='utf-8'))
        main = next((node for node in tree.body if isinstance(node, ast.FunctionDef) and node.name == 'main'), None)
        if main is None:
            if arguments:
                raise ValueError('helper has arguments but no main parser')
        else:
            statements = []
            for node in main.body:
                if isinstance(node, ast.Assign) and isinstance(node.value, ast.Call) and isinstance(node.value.func, ast.Attribute) and node.value.func.attr == 'parse_args':
                    break
                statements.append(node)
            if not any(isinstance(node, ast.Assign) for node in statements):
                if arguments:
                    raise ValueError('helper has no parser')
            else:
                module = ast.fix_missing_locations(ast.Module(body=statements, type_ignores=[]))
                namespace = {'argparse': argparse, '__doc__': '', 'Path': Path, '__file__': str(helper)}
                exec(compile(module, str(helper), 'exec'), namespace)
                namespace['parser'].parse_args(arguments)
        result.append({'helper': helper.relative_to(root).as_posix(), 'arguments': arguments})
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bash', required=True)
    parser.add_argument('--output-dir', required=True)
    args = parser.parse_args()
    root, output = Path(__file__).resolve().parents[2], Path(args.output_dir).resolve()
    output.mkdir(parents=True, exist_ok=True)
    receipt = {'python_ast': [], 'bash_syntax': [], 'references': [], 'cli': [], 'native_execution': False}
    for path in sorted(Path(__file__).parent.glob('v0222_*.py')):
        ast.parse(path.read_text(encoding='utf-8'), filename=str(path))
        receipt['python_ast'].append(path.relative_to(root).as_posix())
    for filename in ('backend-ci.yml', 'publish-personal-ghcr.yml'):
        path = root / '.github/workflows' / filename
        text = path.read_text(encoding='utf-8')
        if 'continue-on-error' in text:
            raise ValueError('CI must not hide failure')
        receipt['references'] += references(root, text)
        for line, script in run_blocks(text):
            target = output / f'{filename}.{line}.sh'
            target.write_text(script, encoding='utf-8', newline='\n')
            result = subprocess.run([args.bash, '--noprofile', '--norc', '-n', target.as_posix()], capture_output=True, timeout=30)
            if result.returncode:
                raise RuntimeError(f'{filename}:{line}: bash -n failed: ' + result.stderr.decode())
            receipt['bash_syntax'].append({'workflow': filename, 'line': line, 'exit_code': result.returncode,
                                          'script_sha256': hashlib.sha256(script.encode()).hexdigest()})
            receipt['cli'] += helper_cli(root, script)
    (output / 'syntax-receipt.json').write_text(json.dumps(receipt, indent=2) + '\n', encoding='utf-8')
    print(f"AST {len(receipt['python_ast'])} helpers; bash -n {len(receipt['bash_syntax'])} blocks; "
          f"CLI {len(receipt['cli'])} invocations; {len(set(receipt['references']))} references passed")


if __name__ == '__main__':
    main()
