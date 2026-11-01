#!/usr/bin/env python3
"""变异自证：证明「存储驱动真机门禁」**能判红**（core#36）。

用法：`python3 scripts/check-live-storage-mutation.py`（在仓根执行）

为什么必须有它：
「真机用例全绿」有两种含义——「实现是对的」与「判据根本没在测东西」。
只有把被测行为**故意改坏**、看门禁会不会红，才能区分这两者。
2026-11-02 的三个真实缺陷（azblob 缺 '?' 分隔符 / canonicalized resource
查询参数形态错 / listAll 不查状态码）就是「单测全绿、真机全线不可用」
——**单测层没有牙齿**，必须由真机门禁 + 变异自证把这条钉死。

纪律（历轮踩坑）：
- **不能只在「绿的时候」跑自证**——必须先让被测物坏掉，看门禁会不会红。
- 每次变异只改一处，跑完立即还原（finally 里无条件还原）。
- 变异期间**真服务必须照常起**——门禁自己起服务（scripts/live-storage-gate.sh），
  变异只改被测源码 ⇒ 判据打到的就是变异后的产物。
- 判据「全灭」时先查「被测物是不是真被执行了」——脚本用「变异后真的红」反证执行到位。

退出码：0=全部变异都被门禁抓住；1=有变异漏网（门禁没牙齿）。
"""
import pathlib
import shutil
import subprocess
import sys

REPO = pathlib.Path(__file__).resolve().parent.parent
OPENDAL = REPO / "storage" / "opendal"
GATE = REPO / "scripts" / "live-storage-gate.sh"

# (名称, 目标文件, 原串, 变异串)
MUTATIONS = [
    (
        "M1 azblob 容器级 URL 不补 '?'（真机 400 InvalidResourceName）",
        OPENDAL / "azblob.go",
        "\t} else {\n\t\tu += \"?\"\n\t}\n\treturn u + query",
        "\t}\n\treturn u + query",
    ),
    (
        "M2 canonicalized resource 查询参数用 name=value（真机 403）",
        OPENDAL / "azblob.go",
        "\t\tif i := strings.Index(kv, \"=\"); i >= 0 {\n\t\t\tkv = kv[:i] + \":\" + kv[i+1:]\n\t\t}",
        "\t\t// 变异：不做 name:value 转换",
    ),
    (
        "M3 listAll 不查 HTTP 状态码（错误被当空列表吞掉）",
        OPENDAL / "azblob.go",
        "\t\tif resp.StatusCode != http.StatusOK {\n\t\t\treturn nil, fmt.Errorf(\"azblob 列举 %s: %d\", prefix, resp.StatusCode)\n\t\t}",
        "\t\t// 变异：不查状态码",
    ),
    (
        "M4 HDFS List 对缺失目录返回错误（与 S3 语义不一致）",
        OPENDAL / "hdfs.go",
        "\tif resp.StatusCode == http.StatusNotFound {\n\t\treturn []*Metadata{}, nil\n\t}",
        "\t// 变异：不再把 404 当空列表",
    ),
]


def run_gate() -> tuple[bool, str]:
    """跑真机门禁。返回 (是否转红, 输出尾部)。"""
    p = subprocess.run(
        ["bash", str(GATE)],
        cwd=str(REPO), capture_output=True, text=True, timeout=1800,
    )
    out = (p.stdout or "") + (p.stderr or "")
    return p.returncode != 0, out


def main() -> int:
    if not GATE.exists():
        print(f"✗ 门禁脚本不存在: {GATE}", file=sys.stderr)
        return 1

    # ── 基线：门禁必须是绿的（否则后面的「变异转红」无意义）──
    print("== 基线：未变异时门禁应为绿 ==")
    red, out = run_gate()
    if red:
        print("✗ 基线就红了 ⇒ 门禁自身不可用，后续自证无意义", file=sys.stderr)
        print(out[-4000:], file=sys.stderr)
        return 1
    print("✓ 基线绿")

    missed: list[str] = []
    for name, target, old, new in MUTATIONS:
        print(f"\n== 变异：{name} ==")
        src = target.read_text(encoding="utf-8")
        if old not in src:
            print(f"✗ 目标串未找到（变异失效，判据可能已陈旧）: {target}", file=sys.stderr)
            missed.append(name)
            continue
        backup = target.with_suffix(target.suffix + ".mutbak")
        shutil.copy2(target, backup)
        try:
            target.write_text(src.replace(old, new, 1), encoding="utf-8")
            red, out = run_gate()
            if red:
                print(f"✓ 转红（门禁抓住了变异）")
                tail = "\n".join(out.strip().splitlines()[-12:])
                print("  " + tail.replace("\n", "\n  "))
            else:
                print(f"✗ 未转红 ⇒ 门禁对这条变异没牙齿", file=sys.stderr)
                missed.append(name)
        finally:
            shutil.move(str(backup), str(target))
            print(f"  （已还原 {target.name}）")

    # ── 还原后再验一次：门禁必须恢复绿（证明还原彻底）──
    print("\n== 还原后复验：门禁应恢复绿 ==")
    red, out = run_gate()
    if red:
        print("✗ 还原后仍红 ⇒ 变异未还原干净", file=sys.stderr)
        print(out[-3000:], file=sys.stderr)
        missed.append("还原复验")
    else:
        print("✓ 还原后绿")

    print("\n== 自证结论 ==")
    if missed:
        print(f"✗ 有 {len(missed)} 条变异未被门禁抓住：{missed}", file=sys.stderr)
        return 1
    print(f"✓ 全部 {len(MUTATIONS)} 条变异都被真机门禁抓住 ⇒ 门禁有牙齿")
    return 0


if __name__ == "__main__":
    sys.exit(main())
