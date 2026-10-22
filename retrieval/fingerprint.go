package retrieval

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
)

// docFile 是语料中一篇待索引文件的定位信息。
type docFile struct {
	relPath string // 相对 corpusDir，统一用 / 分隔（跨平台稳定；进指纹与 doc_id）
	absPath string // 绝对路径，用于读取
}

// listDocs 递归遍历 corpusDir，收集所有常规文件。
// 目录递归进入但不作为文档；符号链接/设备等非常规类型跳过（不计入索引，也不计入 DocsSkipped）。
// 结果按 relPath 排序，保证指纹聚合与插入顺序确定。
func listDocs(corpusDir string) ([]docFile, error) {
	var docs []docFile
	err := filepath.WalkDir(corpusDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(corpusDir, path)
		if err != nil {
			return err
		}
		docs = append(docs, docFile{relPath: filepath.ToSlash(rel), absPath: path})
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
