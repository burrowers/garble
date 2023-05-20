package name

import (
	"github.com/pagran/go-identifiers-database/db"
	"go/token"
	mathrand "math/rand"
	"strings"
)

func capitalize(str string) string {
	return strings.ToUpper(str[:1]) + str[1:]
}

func joinName(arr []string, del string) string {
	if del != "" {
		return strings.Join(arr, del)
	}

	var sb strings.Builder
	for i, s := range arr {
		if i != 0 {
			s = capitalize(s)
		}
		sb.WriteString(s)
	}
	return sb.String()
}

type realisticGenerator struct {
	rnd *mathrand.Rand
}

func (r *realisticGenerator) randomChoice(arr []string, count int) []string {
	res := make([]string, count)
	for i := 0; i < count; i++ {
		res[i] = arr[r.rnd.Intn(len(arr))]
	}
	return res
}

func (r *realisticGenerator) generatePackage(try int) string {
	return "_" + joinName(r.randomChoice(db.GetPackages(), try), "_")
}

func (r *realisticGenerator) generateFile(try int) string {
	return joinName(r.randomChoice(db.GetFilenames(), try), "_")
}

func (r *realisticGenerator) generateName(info *Info, try int) string {
	n := joinName(r.randomChoice(db.GetIdentifiers(), try), "")
	if token.IsExported(info.Name) {
		n = capitalize(n)
	}
	if token.IsKeyword(n) {
		n += "_"
	}
	return n
}

func (r *realisticGenerator) GetName(info *Info, try int) string {
	switch info.Type {
	case Name, Field:
		return r.generateName(info, try)
	case Package:
		return r.generatePackage(try)
	case File:
		return r.generateFile(try)
	default:
		panic("unreachable")
	}
}

func NewRealisticGenerator(seed int64) GetNameFunc {
	g := &realisticGenerator{
		rnd: mathrand.New(mathrand.NewSource(seed)),
	}
	return g.GetName
}
