// Package fileindex 维护"已处理系统目录树"的内存索引，替代落盘时的
// 反复扫盘查重。
//
// 一次 build 要产出多个系统；此前每落一个文件都要把所有候选系统的同路径
// 读盘哈希一遍（一轮又一轮查磁盘）。改为：系统处理完成后将其目录清单
// （相对路径 → 尺寸+mtime 摘要）读进内存一次；此后查重纯内存筛候选，
// 命中时才读那一个候选文件做 SHA256 核验——不一致换下一个候选，全对不上
// 才真正写入新数据。小组的改动路径在落盘后增量并入，让后处理的系统能
// 复用先落盘系统刚写入的独特文件。
package fileindex

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// Entry 是单个文件的轻量摘要。mtime 进摘要是为了 RefreshIfChanged 能用
// O(1) stat 判断"文件是否可能变了"，避免重复读取大文件。
type Entry struct {
	Size     int64
	MtimeSec int64
}

// Index 以 relPath -> 系统根 -> 摘要 的两级结构登记全部已见文件。
type Index struct {
	entries map[string]map[string]Entry
}

// New 创建空索引。
func New() *Index {
	return &Index{entries: map[string]map[string]Entry{}}
}

// ScanSystem 整树扫描 root 并覆盖其在索引中的条目（重新扫描即刷新）。
func (ix *Index) ScanSystem(root string) error {
	for rel := range ix.entries { // 清掉该根的全部旧条目
		delete(ix.entries[rel], root)
	}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		fi, ferr := d.Info()
		if ferr != nil || !fi.Mode().IsRegular() {
			return nil // 与落盘 diff 同口径：只登记常规文件
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		ix.put(root, rel, Entry{Size: fi.Size(), MtimeSec: fi.ModTime().Unix()})
		return nil
	})
	if err != nil {
		return fmt.Errorf("扫描 %s: %w", root, err)
	}
	return nil
}

// Absorb 把 root 下列出的相对路径增量并入索引（小组落盘后的子树记录），
// 不触碰其它路径与其它系统的既有条目。
func (ix *Index) Absorb(root string, rels []string) {
	for _, rel := range rels {
		fi, err := os.Stat(filepath.Join(root, rel))
		if err != nil || !fi.Mode().IsRegular() {
			continue // 落盘后又被清掉的极端情况：不入索引
		}
		ix.put(root, rel, Entry{Size: fi.Size(), MtimeSec: fi.ModTime().Unix()})
	}
}

// Systems 返回索引中已登记的系统根列表（按根路径排序）。
func (ix *Index) Systems() []string {
	out := map[string]bool{}
	for _, roots := range ix.entries {
		for root := range roots {
			out[root] = true
		}
	}
	var list []string
	for r := range out {
		list = append(list, r)
	}
	sort.Strings(list)
	return list
}

// Candidates 返回拥有同路径同尺寸文件的系统根列表（按根路径排序保证确定）。
func (ix *Index) Candidates(rel string, size int64) []string {
	roots := ix.entries[rel]
	if len(roots) == 0 {
		return nil
	}
	var out []string
	for root, e := range roots {
		if e.Size == size {
			out = append(out, root)
		}
	}
	sort.Strings(out)
	return out
}

// RefreshIfChanged 用一次 stat 校验 root 的 rel 是否仍与索引一致；
// 不一致时重读摘要并更新索引。changed=true 表示检测到差异并已刷新。
// 文件已消失时保留旧值不刷新（交给调用方的内容核验兜底）。
func (ix *Index) RefreshIfChanged(root, rel string) (Entry, bool) {
	old, had := ix.entryOf(root, rel)
	fi, err := os.Stat(filepath.Join(root, rel))
	if err != nil || !fi.Mode().IsRegular() {
		return old, false
	}
	cur := Entry{Size: fi.Size(), MtimeSec: fi.ModTime().Unix()}
	if had && same(old, cur) {
		return old, false
	}
	ix.put(root, rel, cur)
	return cur, true
}

func same(a, b Entry) bool { return a.Size == b.Size && a.MtimeSec == b.MtimeSec }

func (ix *Index) put(root, rel string, e Entry) {
	m := ix.entries[rel]
	if m == nil {
		m = map[string]Entry{}
		ix.entries[rel] = m
	}
	m[root] = e
}

func (ix *Index) entryOf(root, rel string) (Entry, bool) {
	e, ok := ix.entries[rel][root]
	return e, ok
}

// SHA256File 计算文件的 sha256 十六进制串。
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
