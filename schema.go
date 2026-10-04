package engine

import (
	"errors"
	"fmt"
)

// FieldSpec 声明一个字段的逻辑类型。
type FieldSpec struct {
	Name string
	Kind Kind
}

// IndexSpec 声明一个二级索引。
type IndexSpec struct {
	Name    string   // 索引名，表内唯一
	Key     []string // 索引键字段（有序）
	Unique  bool     // 是否唯一索引
	Covered []string // 覆盖附加列（除主键与键字段外冗余进索引项的列）
}

// Schema 描述一张表：主键字段、字段类型、二级索引集合。
type Schema struct {
	PrimaryKey string
	Fields     []FieldSpec
	Indexes    []IndexSpec

	fieldMap map[string]Kind
	indexMap map[string]*IndexSpec
}

func (s *Schema) fieldKind(name string) (Kind, bool) {
	if s.fieldMap == nil {
		return 0, false
	}
	k, ok := s.fieldMap[name]
	return k, ok
}

func (s *Schema) index(name string) (*IndexSpec, bool) {
	if s.indexMap == nil {
		return nil, false
	}
	spec, ok := s.indexMap[name]
	return spec, ok
}

// validate 校验 Schema 并构建内部查找表。
func (s *Schema) validate() error {
	if s.PrimaryKey == "" {
		return errors.New("engine: schema requires a primary key field")
	}
	s.fieldMap = make(map[string]Kind, len(s.Fields)+1)
	for _, f := range s.Fields {
		if f.Name == "" {
			return errors.New("engine: field with empty name")
		}
		if _, dup := s.fieldMap[f.Name]; dup {
			return fmt.Errorf("engine: duplicate field %q", f.Name)
		}
		s.fieldMap[f.Name] = f.Kind
	}
	// 主键必须有类型声明（主键参与保序编码）。
	if _, pkDeclared := s.fieldMap[s.PrimaryKey]; !pkDeclared {
		return fmt.Errorf("engine: primary key field %q is not declared in Fields", s.PrimaryKey)
	}

	s.indexMap = make(map[string]*IndexSpec, len(s.Indexes))
	for i := range s.Indexes {
		spec := &s.Indexes[i]
		if spec.Name == "" {
			return errors.New("engine: index with empty name")
		}
		if _, dup := s.indexMap[spec.Name]; dup {
			return fmt.Errorf("engine: duplicate index name %q", spec.Name)
		}
		if len(spec.Key) == 0 {
			return fmt.Errorf("engine: index %q has no key fields", spec.Name)
		}
		seen := map[string]bool{}
		for _, k := range spec.Key {
			if _, ok := s.fieldMap[k]; !ok {
				return fmt.Errorf("engine: index %q references unknown field %q", spec.Name, k)
			}
			if seen[k] {
				return fmt.Errorf("engine: index %q has duplicate key field %q", spec.Name, k)
			}
			seen[k] = true
		}
		for _, c := range spec.Covered {
			if _, ok := s.fieldMap[c]; !ok {
				return fmt.Errorf("engine: index %q covers unknown field %q", spec.Name, c)
			}
			if seen[c] {
				return fmt.Errorf("engine: index %q lists field %q both in key and covered", spec.Name, c)
			}
			seen[c] = true
		}
		s.indexMap[spec.Name] = spec
	}
	return nil
}
