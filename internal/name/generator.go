package name

import (
	"go/token"
	"strings"
	"sync"
)

type PackageInfo struct {
	ImportPath string
}

type FieldInfo struct {
	StructIdentifier string
	Name             string
}

type Generator interface {
	GetPackageName(*PackageInfo) string
	GetFieldName(*FieldInfo) string
}

type shortGenerator struct {
	m          sync.Mutex
	packageMap map[string]string
	fieldsMap  map[string]string
}

func (s *shortGenerator) baseEncodeInt(prefix, charset string, num int) string {
	var sb strings.Builder
	sb.WriteString(prefix)
	for num > 0 {
		r := num % len(charset)
		num /= len(charset)

		sb.WriteByte(charset[r])
	}
	return sb.String()
}

const (
	lowCharset  = "abcdefghijklmnopqrstuvwxyz0123456789"
	fullCharset = lowCharset + "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
)

func (s *shortGenerator) GetPackageName(info *PackageInfo) string {
	s.m.Lock()
	defer s.m.Unlock()

	key := info.ImportPath
	newName, ok := s.packageMap[key]
	if !ok {
		newName = s.baseEncodeInt("p", lowCharset, len(s.packageMap)+1)
		s.packageMap[key] = newName
	}
	return newName
}

func (s *shortGenerator) GetFieldName(info *FieldInfo) string {
	s.m.Lock()
	defer s.m.Unlock()

	key := info.StructIdentifier + "\x00" + info.Name
	newName, ok := s.packageMap[key]
	if !ok {
		prefix := "f"
		if token.IsExported(info.Name) {
			prefix = "F"
		}
		newName = s.baseEncodeInt(prefix, fullCharset, len(s.packageMap)+1)
		if token.IsKeyword(newName) {
			newName += "_"
		}
		s.packageMap[key] = newName
	}
	return newName
}

func NewShortGenerator() Generator {
	return &shortGenerator{
		packageMap: make(map[string]string),
		fieldsMap:  make(map[string]string),
	}
}
