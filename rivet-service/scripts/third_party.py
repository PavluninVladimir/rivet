#!/usr/bin/env python3
"""NOTICE и THIRD_PARTY в корне репозитория rivet.

Собирает лицензии сторонних компонентов, которые распространяются вместе с
Rivet:
- Go-модули rivetd, rivet-runner и CLI rivet. Список и типы лицензий даёт
  go-licenses (GOOS linux и darwin), тексты лицензий и NOTICE берутся из
  кеша модулей.
- Пакеты npm веб-консоли, встроенной в rivetd. Источник: сама встроенная
  сборка internal/webui/dist: third-party-licenses.md (vite build.license)
  и NOTICE, которые кладёт туда сборка rivet-web. Так файлы описывают ровно
  то, что распространяется, а не соседний клон rivet-web.

Запуск из rivet-service: make third-party (после make ui). Нужны
go-licenses в PATH (или в GO_LICENSES). Если во встроенной сборке ещё нет
этих файлов (консоль собрана до open-source), разделы консоли берутся из
текущих THIRD_PARTY и NOTICE.

--check ничего не пишет: сверяет файлы с тем, что получилось бы, и
завершается с кодом 1, если они устарели (так проверяет CI).
"""

import json
import os
import re
import subprocess
import sys

OWN_MODULE = "github.com/PavluninVladimir/rivet"
SERVICE_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
ROOT = os.path.dirname(SERVICE_DIR)
WEB_DIST = os.path.join(SERVICE_DIR, "internal", "webui", "dist")
LICENSE_RE = re.compile(r"^(LICEN[CS]E|COPYING)([-._][\w.-]*)?(?<!\.go)$", re.I)
NOTICE_RE = re.compile(r"^NOTICE(\.(md|txt))?$", re.I)
RULE = "-" * 78
WEB_MARK = "Веб-консоль (пакеты npm, встроены в rivetd)"
WEB_NOTICE_MARK = "Web console (npm packages embedded into rivetd)"


def run(args, env=None):
    return subprocess.run(args, cwd=SERVICE_DIR, env=env, check=True,
                          capture_output=True, text=True).stdout


def go_env(goos):
    env = dict(os.environ, GOOS=goos)
    # go-licenses определяет стандартную библиотеку по GOROOT: он должен
    # совпадать с инструментарием, который выбрал go.mod.
    env["GOROOT"] = run(["go", "env", "GOROOT"]).strip()
    env["PATH"] = os.path.join(env["GOROOT"], "bin") + os.pathsep + env.get("PATH", "")
    return env


def libraries():
    """Библиотеки (путь, где найдена лицензия) и их SPDX-идентификаторы."""
    tool = os.environ.get("GO_LICENSES", "go-licenses")
    libs = {}
    for goos in ("linux", "darwin"):
        out = run([tool, "report", "./...", "--ignore", OWN_MODULE], env=go_env(goos))
        for line in out.splitlines():
            parts = line.split(",")
            if len(parts) < 3:
                continue
            libs.setdefault(parts[0], set()).add(parts[2])
    return libs


def modules():
    """Модули зависимостей: путь → (версия, каталог в кеше)."""
    out = run(["go", "list", "-m", "-json", "all"])
    mods = {}
    dec = json.JSONDecoder()
    i = 0
    while i < len(out):
        while i < len(out) and out[i].isspace():
            i += 1
        if i >= len(out):
            break
        m, i = dec.raw_decode(out, i)
        if m.get("Main") or not m.get("Dir"):
            continue
        mods[m["Path"]] = (m.get("Version", ""), m["Dir"])
    return mods


def module_of(lib, mods):
    best = ""
    for path in mods:
        if (lib == path or lib.startswith(path + "/")) and len(path) > len(best):
            best = path
    return best


def find_files(start, stop, pattern):
    """Файлы по шаблону: от каталога библиотеки вверх до корня модуля."""
    d = start
    while True:
        if os.path.isdir(d):
            found = sorted(f for f in os.listdir(d) if pattern.match(f))
            if found:
                return [os.path.join(d, f) for f in found]
        if os.path.normpath(d) == os.path.normpath(stop):
            return []
        d = os.path.dirname(d)


def read(path):
    with open(path, encoding="utf-8", errors="replace") as f:
        return f.read().replace("\r\n", "\n").strip("\n")


def go_sections():
    libs = libraries()
    mods = modules()
    third, notices = [], []
    seen_notice = set()
    for lib in sorted(libs):
        mod = module_of(lib, mods)
        if not mod:
            sys.exit(f"third_party: модуль библиотеки {lib} не найден в go list -m all")
        version, mdir = mods[mod]
        ldir = os.path.join(mdir, os.path.relpath(lib, mod)) if lib != mod else mdir
        lic = find_files(ldir, mdir, LICENSE_RE)
        if not lic:
            sys.exit(f"third_party: у {lib} нет файла лицензии в {mdir}")
        ids = ", ".join(sorted(libs[lib]))
        body = "\n\n".join(read(p) for p in lic)
        third.append(f"{RULE}\n{lib} {version}\nLicense: {ids}\n{RULE}\n{body}\n")
        for p in find_files(ldir, mdir, NOTICE_RE):
            if p in seen_notice:
                continue
            seen_notice.add(p)
            notices.append(f"{RULE}\n{mod} {version}: {os.path.relpath(p, mdir)}\n{RULE}\n{read(p)}\n")
    return third, notices


def web_part(name, start, existing, mark):
    """Раздел консоли из файла встроенной сборки, начиная со строки start,
    иначе из текущего файла rivet после заголовка mark."""
    src = os.path.join(WEB_DIST, name)
    if os.path.exists(src):
        text = read(src)
        k = text.find(start)
        if k < 0:
            sys.exit(f"third_party: в {src} нет раздела пакетов: пересоберите консоль (make ui)")
        return text[k:] + "\n"
    if mark in existing:
        return existing.split(mark, 1)[1].split("\n", 2)[2].lstrip("\n")
    sys.exit(f"third_party: нет {src}: соберите консоль во встроенную статику (make ui)")


def main():
    check = "--check" in sys.argv[1:]
    third_path, notice_path = os.path.join(ROOT, "THIRD_PARTY"), os.path.join(ROOT, "NOTICE")
    existing = read(third_path) + "\n" if os.path.exists(third_path) else ""
    existing_notice = read(notice_path) + "\n" if os.path.exists(notice_path) else ""
    third, notices = go_sections()
    web = web_part("third-party-licenses.md", "\n## ", existing, WEB_MARK).lstrip("\n")
    web_notice = web_part("NOTICE", RULE, existing_notice, WEB_NOTICE_MARK)

    third_text = (
        "THIRD-PARTY SOFTWARE NOTICES AND LICENSES\n"
        "=========================================\n\n"
        "Third-party components distributed with Rivet (rivetd, rivet-runner, the\n"
        "rivet CLI and the web console embedded into rivetd) and their licenses.\n"
        "Generated by `make third-party` in rivet-service; do not edit by hand.\n\n"
        "Сторонние компоненты, которые распространяются вместе с Rivet, и тексты\n"
        "их лицензий. Файл собирает `make third-party`, руками не правится.\n\n"
        "Go-модули (rivetd, rivet-runner, CLI rivet)\n"
        "===========================================\n\n"
        + "\n".join(third)
        + "\n" + WEB_MARK + "\n" + "=" * len(WEB_MARK) + "\n\n"
        + web
    )
    notice_text = (
        "Rivet\n"
        "Copyright (C) 2026 Vladimir Pavlunin\n\n"
        "This product includes third-party components licensed under the Apache\n"
        "License 2.0 and other licenses whose NOTICE files must be reproduced.\n"
        "Their notices follow; full license texts are in THIRD_PARTY.\n"
        "Generated by `make third-party` in rivet-service; do not edit by hand.\n\n"
        "Go modules\n==========\n\n"
        + "\n".join(notices)
        + "\n" + WEB_NOTICE_MARK + "\n" + "=" * len(WEB_NOTICE_MARK) + "\n\n"
        + web_notice
    )
    stale = []
    for path, text in ((third_path, third_text), (notice_path, notice_text)):
        old = read(path) + "\n" if os.path.exists(path) else ""
        if old == text:
            continue
        stale.append(os.path.basename(path))
        if not check:
            with open(path, "w", encoding="utf-8") as f:
                f.write(text)
    if check and stale:
        sys.exit("third_party: устарели " + ", ".join(stale) + ": выполните make third-party в rivet-service")


if __name__ == "__main__":
    main()
