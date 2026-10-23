package retrieval

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// docFile 是语料中一篇待索引文件的定位信息。
type docFile struct {
	relPath string // 相对 corpusDir，统一用 / 分隔（跨平台稳定；进指纹与 doc_id）
	absPath string // 绝对路径，用于读取
}

// corpusExt 是语料收录的唯一后缀（大小写不敏感）：只收 .md。
// 为什么限定 .md（harness 缺陷修复，勿回退）：二进制文件（尤其是检索索引库自身的
// .db/.db-wal）一旦进语料就形成正反馈自噬循环——库大→索引的 chunk 多→库更大，
// 且库内容每次写入都变，corpus 指纹永远不符→每次启动全量 Rebuild。
// 实测 agent-runtime.db 自身产生 293409 行、.db-wal 114195 行，411965 行语料中
// 仅 ~35402 是正常 md，纯词法组 recall 被稀释到 0。
const corpusExt = ".md"

// evalDirPrefixes 是被排除的评测产物目录名序列（相对 corpusDir，逐段大小写不敏感）：
// corpusDir=仓库根 时是 docs/eval/，corpusDir=docs 时是 eval/（main.go 两个默认值）。
// 评测报表在评测期间被写进语料目录，会改变语料指纹、触发下次无谓全量 Rebuild（缺陷 C），
// 因此评测产物一律不算语料。
var evalDirPrefixes = [][]string{
	{"docs", "eval"},
	{"eval"},
}

// underEvalDir 判断 relPath（/ 分隔）是否位于评测产物目录内。
func underEvalDir(relPath string) bool {
	segs := strings.Split(relPath, "/")
	for _, prefix := range evalDirPrefixes {
		if len(segs) <= len(prefix) {
			continue // 目录自身或更高层不算「目录内」，只排除其子孙
		}
		match := true
		for i, want := range prefix {
			if !strings.EqualFold(segs[i], want) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// isHiddenPath 判断 relPath（/ 分隔）任一路径段是否以 . 开头：隐藏文件/隐藏目录。
// 覆盖 .git/.codebuddy/.claude/.trae/.pi/.idea/.worktrees 等目录，
// 以及根下 .agent-context.md/.agent-rules.md 等隐藏文件。
// 为什么必须排除（Task 12 缺陷 D 实证）：隐藏文件/目录由 IDE/agent 会话持续改写，
// 入语料会让指纹永不稳定 —— 真实语料 49 个 .md 中 9 个是此类隐藏路径，
// 内容一变 CheckFresh 就恒返回 ErrCorpusStale，每次 search_docs 都失败
// （评测 72/90 wrong-span 全属假红）。含隐藏=4fccc8a9 / 不含=c5c45336 已逐字节复现。
func isHiddenPath(relPath string) bool {
	for _, seg := range strings.Split(relPath, "/") {
		if seg == "." || seg == ".." {
			continue // WalkDir 根目录的 rel 就是 "."，不是隐藏路径（否则整棵语料被跳过）
		}
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

// listDocs 递归遍历 corpusDir，收集语料文件：只收 .md（大小写不敏感），
// 排除 docs/eval/ 评测产物目录与隐藏文件/目录（见 isHiddenPath 注释）。
// 目录递归进入但不作为文档；符号链接/设备等非常规类型跳过（不计入索引，也不计入 DocsSkipped）。
// 结果按 relPath 排序，保证指纹聚合与插入顺序确定。
func listDocs(corpusDir string) ([]docFile, error) {
	var docs []docFile
	err := filepath.WalkDir(corpusDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(corpusDir, path)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		if isHiddenPath(relSlash) {
			if d.IsDir() {
				return fs.SkipDir // 整棵隐藏子树都不可能是语料（.git/.worktrees 等）
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(relSlash), corpusExt) {
			return nil // 非 .md：不入语料（二进制自索引会形成正反馈，见 corpusExt 注释）
		}
		if underEvalDir(relSlash) {
			return nil // 评测产物不是语料（见 evalDirPrefixes 注释）
		}
		docs = append(docs, docFile{relPath: relSlash, absPath: path})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].relPath < docs[j].relPath })
	return docs, nil
}

// docHash 对单篇文档全文算 SHA-256（内容寻址：只取决于内容字节，与路径/mtime 无关）。
func docHash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// aggregateFingerprint 把各 (relPath, docHash) 按路径排序后拼接再 SHA-256。
// 聚合同时受「内容」与「路径集合」影响：改任一文档内容或增删文档都会改指纹。
// mtime/权限等元数据一律不参与（git checkout / 跨机 mtime 漂移不误判陈旧，ADR-0017 决策 6）。
func aggregateFingerprint(entries map[string]string) string {
	paths := make([]string, 0, len(entries))
	for p := range entries {
		paths = append(paths, p)
	}
	sort.Strings(paths) // 稳定聚合：与 map 迭代顺序无关
	h := sha256.New()
	for _, p := range paths {
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write([]byte(entries[p]))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// dirFingerprint 遍历 corpusDir，对每个可读文件算内容 SHA-256，与相对路径一起聚合成目录指纹。
// 单文件读失败：跳过（不计入指纹，与 Rebuild 的跳过语义一致），slog.Warn 记录可观测。
func dirFingerprint(corpusDir string) (string, error) {
	docs, err := listDocs(corpusDir)
	if err != nil {
		return "", err
	}
	entries := make(map[string]string, len(docs))
	for _, d := range docs {
		content, err := os.ReadFile(d.absPath)
		if err != nil {
			slog.Warn("retrieval: fingerprint skip unreadable doc", "path", d.relPath, "error", err)
			continue
		}
		entries[d.relPath] = docHash(content)
	}
	return aggregateFingerprint(entries), nil
}
