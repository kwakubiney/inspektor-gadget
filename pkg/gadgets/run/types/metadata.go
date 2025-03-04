// Copyright 2023 The Inspektor Gadget authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package types

import (
	"errors"
	"fmt"
	"strings"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/btf"
	"github.com/hashicorp/go-multierror"
	log "github.com/sirupsen/logrus"

	"github.com/inspektor-gadget/inspektor-gadget/pkg/columns"
	"github.com/inspektor-gadget/inspektor-gadget/pkg/gadgets"
)

// Keep this aligned with include/gadget/macros.h
const (
	// Prefix used to mark trace maps
	traceMapPrefix = "gadget_trace_map_"
)

const (
	DefaultColumnWidth = 16
)

type Alignment string

const (
	AlignmenNone   Alignment = ""
	AlignmentLeft  Alignment = "left"
	AlignmentRight Alignment = "right"
)

type EllipsisType string

const (
	EllipsisNone   EllipsisType = ""
	EllipsisStart  EllipsisType = "start"
	EllipsisMiddle EllipsisType = "middle"
	EllipsisEnd    EllipsisType = "end"
)

// FieldAttributes describes how to format a field. It's almost 1:1 mapping with columns.Attributes,
// however we are keeping this separated because we don't want to create a strong coupling with the
// columns library now. Later on we can consider merging both of them.
type FieldAttributes struct {
	// Width to reserve for this field
	Width uint `yaml:"width,omitempty"`
	// MinWidth is the minimum width for this field
	MinWidth uint `yaml:"minWidth,omitempty"`
	// MaxWidth is the maximum width for this field
	MaxWidth uint `yaml:"maxWidth,omitempty"`
	// Alignment of this column (left or right)
	Alignment Alignment `yaml:"alignment,omitempty"`
	// Hidden defines whether a column is to be hid by default
	Hidden bool `yaml:"hidden,omitempty"`
	// EllipsisType defines how to abbreviate this column if the value needs more space than is
	// available. (start, middle or end)
	Ellipsis EllipsisType `yaml:"ellipsis,omitempty"`
	// Template defines the template that will be used.
	// TODO: add a link to existing templates
	Template string `yaml:"template,omitempty"`
}

type Field struct {
	// Field name
	Name string `yaml:"name"`
	// Field description
	Description string `yaml:"description,omitempty"`
	// Attributes defines how the field should be formatted
	Attributes FieldAttributes `yaml:"attributes"`
	// Annotations represents extra information that is not relevant to Inspektor Gadget, but
	// for other applications, like color font for instance.
	Annotations map[string]interface{} `yaml:"annotations,omitempty"`
}

// Struct describes a type generated by the gadget
type Struct struct {
	Fields []Field `yaml:"fields"`
}

// Tracer describe the behavior of a gadget that collects and sends events to user space
// TODO: We need to rename this concept not to collide with the opentelemetry concept
type Tracer struct {
	// Name of the perf event array or ring buffer that the gadget uses to send events
	MapName string `yaml:"mapName"`
	// Name of the structure generated by this tracer
	StructName string `yaml:"structName"`
}

type GadgetMetadata struct {
	// Gadget name
	Name string `yaml:"name"`
	// Gadget description
	Description string `yaml:"description,omitempty"`
	// Tracers implemented by the gadget
	// TODO: Rename this field to something that doesn't collide with the opentelemetry concept
	Tracers map[string]Tracer `yaml:"tracers,omitempty"`
	// Types generated by the gadget
	Structs map[string]Struct `yaml:"structs,omitempty"`
}

func (m *GadgetMetadata) Validate(spec *ebpf.CollectionSpec) error {
	var result error

	if m.Name == "" {
		result = multierror.Append(result, errors.New("gadget name is required"))
	}

	if err := m.validateTracers(spec); err != nil {
		result = multierror.Append(result, err)
	}

	if err := m.validateStructs(spec); err != nil {
		result = multierror.Append(result, err)
	}

	return result
}

func (m *GadgetMetadata) validateTracers(spec *ebpf.CollectionSpec) error {
	var result error

	// Temporary limitation
	if len(m.Tracers) > 1 {
		result = multierror.Append(result, errors.New("only one tracer is allowed"))
	}

	for name, tracer := range m.Tracers {
		if tracer.MapName == "" {
			result = multierror.Append(result, fmt.Errorf("tracer %q is missing mapName", name))
		}

		if tracer.StructName == "" {
			result = multierror.Append(result, fmt.Errorf("tracer %q is missing structName", name))
		}

		_, ok := m.Structs[tracer.StructName]
		if !ok {
			result = multierror.Append(result, fmt.Errorf("tracer %q references unknown struct %q", name, tracer.StructName))
		}

		ebpfm, ok := spec.Maps[tracer.MapName]
		if !ok {
			result = multierror.Append(result, fmt.Errorf("map %q not found in eBPF object", tracer.MapName))
			continue
		}

		if err := validateTraceMap(ebpfm); err != nil {
			result = multierror.Append(result, err)
		}
	}

	return result
}

func validateTraceMap(traceMap *ebpf.MapSpec) error {
	if traceMap.Type != ebpf.RingBuf && traceMap.Type != ebpf.PerfEventArray {
		return fmt.Errorf("map %q has a wrong type, expected: ringbuf or perf event array, got: %s",
			traceMap.Name, traceMap.Type.String())
	}

	if traceMap.Value == nil {
		return fmt.Errorf("map %q does not have BTF information its value", traceMap.Name)
	}

	if _, ok := traceMap.Value.(*btf.Struct); !ok {
		return fmt.Errorf("value of BPF map %q is not a structure", traceMap.Name)
	}

	return nil
}

func (m *GadgetMetadata) validateStructs(spec *ebpf.CollectionSpec) error {
	var result error

	for name, mapStruct := range m.Structs {
		var btfStruct *btf.Struct
		if err := spec.Types.TypeByName(name, &btfStruct); err != nil {
			result = multierror.Append(result, fmt.Errorf("looking for struct %q in eBPF object: %w", name, err))
			continue
		}

		mapStructFields := make(map[string]Field, len(mapStruct.Fields))
		for _, f := range mapStruct.Fields {
			mapStructFields[f.Name] = f
		}

		btfStructFields := make(map[string]btf.Member, len(btfStruct.Members))
		for _, m := range btfStruct.Members {
			btfStructFields[m.Name] = m
		}

		for fieldName := range mapStructFields {
			if _, ok := btfStructFields[fieldName]; !ok {
				result = multierror.Append(result, fmt.Errorf("field %q not found in eBPF struct %q", fieldName, name))
			}
		}
	}

	return result
}

// Populate fills the metadata from its ebpf spec
func (m *GadgetMetadata) Populate(spec *ebpf.CollectionSpec) error {
	if m.Name == "" {
		m.Name = "TODO: Fill the gadget name"
	}

	if m.Description == "" {
		m.Description = "TODO: Fill the gadget description"
	}

	if m.Tracers == nil {
		m.Tracers = make(map[string]Tracer)
	}

	if m.Structs == nil {
		m.Structs = make(map[string]Struct)
	}

	if err := m.populateTracers(spec); err != nil {
		return fmt.Errorf("handling trace maps: %w", err)
	}

	return nil
}

func getUnderlyingType(tf *btf.Typedef) (btf.Type, error) {
	switch typedMember := tf.Type.(type) {
	case *btf.Typedef:
		return getUnderlyingType(typedMember)
	default:
		return typedMember, nil
	}
}

func getColumnSize(typ btf.Type) uint {
	switch typedMember := typ.(type) {
	case *btf.Int:
		switch typedMember.Encoding {
		case btf.Signed:
			switch typedMember.Size {
			case 1:
				return columns.MaxCharsInt8
			case 2:
				return columns.MaxCharsInt16
			case 4:
				return columns.MaxCharsInt32
			case 8:
				return columns.MaxCharsInt64

			}
		case btf.Unsigned:
			switch typedMember.Size {
			case 1:
				return columns.MaxCharsUint8
			case 2:
				return columns.MaxCharsUint16
			case 4:
				return columns.MaxCharsUint32
			case 8:
				return columns.MaxCharsUint64
			}
		case btf.Bool:
			return columns.MaxCharsBool
		case btf.Char:
			return columns.MaxCharsChar
		}
	case *btf.Typedef:
		typ, _ := getUnderlyingType(typedMember)
		return getColumnSize(typ)
	}

	return DefaultColumnWidth
}

func (m *GadgetMetadata) populateTracers(spec *ebpf.CollectionSpec) error {
	traceMap := getTracerMapFromeBPF(spec)
	if traceMap == nil {
		log.Debug("No trace map found")
		return nil
	}

	if err := validateTraceMap(traceMap); err != nil {
		return fmt.Errorf("trace map is invalid: %w", err)
	}

	traceMapStruct := traceMap.Value.(*btf.Struct)

	found := false

	// TODO: this is weird but we need to check the map name as the tracer name can be
	// different.
	for _, t := range m.Tracers {
		if t.MapName == traceMap.Name {
			found = true
			break
		}
	}

	if !found {
		log.Debugf("Adding tracer %q", traceMap.Name)
		m.Tracers[traceMap.Name] = Tracer{
			MapName:    traceMap.Name,
			StructName: traceMapStruct.Name,
		}
	} else {
		log.Debugf("Tracer using map %q already defined, skipping", traceMap.Name)
	}

	if err := m.populateStruct(traceMapStruct); err != nil {
		return fmt.Errorf("populating struct: %w", err)
	}

	return nil
}

// getGadgetIdentByPrefix returns the string generated by GADGET_ macros.
func getGadgetIdentByPrefix(spec *ebpf.CollectionSpec, prefix string) string {
	it := spec.Types.Iterate()
	for it.Next() {
		v, ok := it.Type.(*btf.Var)
		if !ok {
			continue
		}

		if strings.HasPrefix(v.Name, prefix) {
			return strings.TrimPrefix(v.Name, prefix)
		}
	}

	return ""
}

// getTracerMapFromeBPF returns the tracer map from the eBPF object.
// It looks for maps marked with GADGET_TRACE_MAP() and returns the first one.
func getTracerMapFromeBPF(spec *ebpf.CollectionSpec) *ebpf.MapSpec {
	mapName := getGadgetIdentByPrefix(spec, traceMapPrefix)
	return spec.Maps[mapName]
}

func (m *GadgetMetadata) populateStruct(btfStruct *btf.Struct) error {
	if m.Structs == nil {
		m.Structs = make(map[string]Struct)
	}

	gadgetStruct := m.Structs[btfStruct.Name]
	existingFields := make(map[string]struct{})
	for _, field := range gadgetStruct.Fields {
		existingFields[field.Name] = struct{}{}
	}

	for _, member := range btfStruct.Members {
		// skip some specific members
		if member.Name == "timestamp" {
			log.Debug("Ignoring timestamp field: see https://github.com/inspektor-gadget/inspektor-gadget/issues/2000")
			continue
		}
		// TODO: temporary disable mount ns as it'll be duplicated otherwise
		if member.Type.TypeName() == gadgets.MntNsIdTypeName {
			continue
		}

		// check if field already exists
		if _, ok := existingFields[member.Name]; ok {
			log.Debugf("Field %q already exists, skipping", member.Name)
			continue
		}

		log.Debugf("Adding field %q", member.Name)
		field := Field{
			Name:        member.Name,
			Description: "TODO: Fill field description",
			Attributes: FieldAttributes{
				Width:     getColumnSize(member.Type),
				Alignment: AlignmentLeft,
				Ellipsis:  EllipsisEnd,
			},
		}

		gadgetStruct.Fields = append(gadgetStruct.Fields, field)
	}

	m.Structs[btfStruct.Name] = gadgetStruct

	return nil
}
