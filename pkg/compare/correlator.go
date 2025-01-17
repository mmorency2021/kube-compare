// SPDX-License-Identifier:Apache-2.0

package compare

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/klog/v2"
)

var FieldSeparator = "_"

// Correlator provides an abstraction that allow the usage of different Resource correlation logics
// in the kubectl cluster-compare. The correlation process Matches for each Resource a template.
type Correlator interface {
	Match(*unstructured.Unstructured) ([]*ReferenceTemplate, error)
}

// UnknownMatch an error that can be returned by a Correlator in a case no template was matched for a Resource.
type UnknownMatch struct {
	Resource *unstructured.Unstructured
}

func (e UnknownMatch) Error() string {
	return fmt.Sprintf("Template couldn't be matched for: %s", apiKindNamespaceName(e.Resource))
}

func apiKindNamespaceName(r *unstructured.Unstructured) string {
	if r.GetNamespace() == "" {
		return strings.Join([]string{r.GetAPIVersion(), r.GetKind(), r.GetName()}, FieldSeparator)
	}
	return strings.Join([]string{r.GetAPIVersion(), r.GetKind(), r.GetNamespace(), r.GetName()}, FieldSeparator)
}

// MultiCorrelator Matches templates by attempting to find a match with one of its predefined Correlators.
type MultiCorrelator struct {
	correlators []Correlator
}

func NewMultiCorrelator(correlators []Correlator) *MultiCorrelator {
	return &MultiCorrelator{correlators: correlators}
}

func (c MultiCorrelator) Match(object *unstructured.Unstructured) ([]*ReferenceTemplate, error) {
	var errs []error
	for _, core := range c.correlators {
		temp, err := core.Match(object)
		if err == nil || !errors.As(err, &UnknownMatch{}) {
			return temp, err // nolint:wrapcheck
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...) // nolint:wrapcheck
}

// ExactMatchCorrelator Matches templates by exact match between a predefined config including pairs of Resource names and there equivalent template.
// The names of the resources are in the apiVersion-kind-namespace-name format.
// For fields that are not namespaced apiVersion-kind-name format will be used.
type ExactMatchCorrelator struct {
	apiKindNamespaceName map[string]*ReferenceTemplate
}

func NewExactMatchCorrelator(crToTemplate map[string]string, templates []*ReferenceTemplate) (*ExactMatchCorrelator, error) {
	core := ExactMatchCorrelator{}
	core.apiKindNamespaceName = make(map[string]*ReferenceTemplate)
	nameToTemplate := make(map[string]*ReferenceTemplate)
	for _, temp := range templates {
		nameToTemplate[temp.Name()] = temp
	}
	for cr, temp := range crToTemplate {
		templateObj, ok := nameToTemplate[temp]
		if !ok {
			return nil, fmt.Errorf("error in template manual matching for resource: %s no template in the name of %s", cr, temp)
		}
		core.apiKindNamespaceName[cr] = templateObj

	}
	return &core, nil
}

func (c ExactMatchCorrelator) Match(object *unstructured.Unstructured) ([]*ReferenceTemplate, error) {
	temp, ok := c.apiKindNamespaceName[apiKindNamespaceName(object)]
	if !ok {
		return nil, UnknownMatch{Resource: object}
	}
	return []*ReferenceTemplate{temp}, nil
}

// GroupCorrelator Matches templates by hashing predefined fields.
// All The templates are indexed by  hashing groups of `indexed` fields. The `indexed` fields can be nested.
// Resources will be attempted to be matched with hashing by the group with the largest amount of `indexed` fields.
// In case a Resource Matches by a hash a group of templates the group correlator will continue looking for a match
// (with groups with less `indexed fields`) until it finds a distinct match, in case it doesn't, MultipleMatches error
// will be returned.
// Templates will be only indexed by a group of fields only if all fields in group are not templated.
type GroupCorrelator struct {
	fieldCorrelators []*FieldCorrelator
}

// NewGroupCorrelator creates a new GroupCorrelator using inputted fieldGroups and generated GroupFunctions and templatesByGroups.
// The templates will be divided into different kinds of groups based on the fields that are templated. Templates will be added
// to the kind of group that contains the biggest amount of fully defined `indexed` fields.
// For fieldsGroups =  {{{"metadata", "namespace"}, {"kind"}}, {{"kind"}}} and the following templates: [fixedKindTemplate, fixedNamespaceKindTemplate]
// the fixedNamespaceKindTemplate will be added to a mapping where the keys are  in the format of `namespace_kind`. The fixedKindTemplate
// will be added to a mapping where the keys are  in the format of `kind`.
func NewGroupCorrelator(fieldGroups [][][]string, templates []*ReferenceTemplate) (*GroupCorrelator, error) {
	core := GroupCorrelator{}
	objects := templates
	for _, group := range fieldGroups {
		fc := FieldCorrelator{Fields: group, hashFunc: createGroupHashFunc(group)}
		newObjects := fc.ClaimTemplates(objects)

		// Ignore if the fc didn't take any objects
		if len(newObjects) == len(objects) {
			continue
		}

		objects = newObjects
		core.fieldCorrelators = append(core.fieldCorrelators, &fc)

		err := fc.ValidateTemplates()
		if err != nil {
			klog.Warning(err)
		}

	}
	return &core, nil
}

func getFields(fields [][]string) string {
	var stringifiedFields []string
	for _, field := range fields {
		stringifiedFields = append(stringifiedFields, strings.Join(field, FieldSeparator))
	}
	return strings.Join(stringifiedFields, ", ")
}

type templateHashFunc func(*unstructured.Unstructured, string) (group string, err error)

// createGroupHashFunc creates a hashing function for a specific field group
func createGroupHashFunc(fieldGroup [][]string) templateHashFunc {
	groupHashFunc := func(cr *unstructured.Unstructured, replaceEmptyWith string) (group string, err error) {
		var values []string
		for _, fields := range fieldGroup {
			value, isFound, NotStringErr := unstructured.NestedString(cr.Object, fields...)
			if !isFound {
				return "", fmt.Errorf("the field %s doesn't exist in resource", strings.Join(fields, FieldSeparator))
			}
			if NotStringErr != nil {
				return "", fmt.Errorf("the field %s isn't string - grouping by non string values isn't supported", strings.Join(fields, FieldSeparator))
			}
			values = append(values, value)
		}
		return strings.Join(values, FieldSeparator), nil
	}
	return groupHashFunc
}

func getTemplatesNames(templates []*ReferenceTemplate) string {
	var names []string
	for _, temp := range templates {
		names = append(names, temp.Name())
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func (c *GroupCorrelator) Match(object *unstructured.Unstructured) ([]*ReferenceTemplate, error) {
	for _, fc := range c.fieldCorrelators {
		temp, err := fc.Match(object)
		if err != nil {
			continue
		}
		return temp, nil
	}
	return nil, UnknownMatch{Resource: object}
}

// MetricsTracker Matches templates by using an existing correlator and gathers summary info related the correlation.
type MetricsTracker struct {
	UnMatchedCRs          []*unstructured.Unstructured
	unMatchedLock         sync.Mutex
	MatchedTemplatesNames map[string]bool
	matchedLock           sync.Mutex
}

func NewMetricsTracker() *MetricsTracker {
	cr := MetricsTracker{
		UnMatchedCRs:          []*unstructured.Unstructured{},
		MatchedTemplatesNames: map[string]bool{},
	}
	return &cr
}

// containOnly checks if at least one of the joined errors isn't from the err-types passed in errTypes
func containOnly(err error, errTypes []error) bool {
	var errs []error
	joinedErr, isJoined := err.(interface{ Unwrap() []error })
	if isJoined {
		errs = joinedErr.Unwrap()
	} else {
		errs = []error{err}
	}
	for _, errPart := range errs {
		c := false
		for _, errType := range errTypes {
			if reflect.TypeOf(errType).Name() == reflect.TypeOf(errPart).Name() {
				c = true
			}
		}
		if !c {
			return false
		}
	}
	return true
}

func (c *MetricsTracker) addMatch(temp *ReferenceTemplate) {
	c.matchedLock.Lock()
	c.MatchedTemplatesNames[temp.Name()] = true
	c.matchedLock.Unlock()
}

func (c *MetricsTracker) addUNMatch(cr *unstructured.Unstructured) {
	c.unMatchedLock.Lock()
	c.UnMatchedCRs = append(c.UnMatchedCRs, cr)
	c.unMatchedLock.Unlock()
}

type FieldCorrelator struct {
	Fields    [][]string
	hashFunc  templateHashFunc
	templates map[string][]*ReferenceTemplate
}

func (f *FieldCorrelator) ClaimTemplates(templates []*ReferenceTemplate) []*ReferenceTemplate {
	if f.templates == nil {
		f.templates = make(map[string][]*ReferenceTemplate)
	}

	discarded := make([]*ReferenceTemplate, 0)
	for _, temp := range templates {
		hash, err := f.hashFunc(temp.metadata, noValue)
		if err != nil || strings.Contains(hash, noValue) {
			discarded = append(discarded, temp)
		} else {
			f.templates[hash] = append(f.templates[hash], temp)
		}
	}

	return discarded
}

func (f *FieldCorrelator) ValidateTemplates() error {
	errs := make([]error, 0)
	for _, values := range f.templates {
		if len(values) > 1 {
			errs = append(errs, fmt.Errorf(
				"More then one template with same %s. By Default for each Cluster CR that is correlated "+
					"to one of these templates the template with the least number of diffs will be used. "+
					"To use a different template for a specific CR specify it in the diff-config (-c flag) "+
					"Template names are: %s",
				getFields(f.Fields), getTemplatesNames(values)),
			)
		}
	}

	return errors.Join(errs...)
}

func (f FieldCorrelator) Match(object *unstructured.Unstructured) ([]*ReferenceTemplate, error) {
	group_hash, err := f.hashFunc(object, "")
	if err != nil {
		return nil, err
	}
	templates, ok := f.templates[group_hash]
	if !ok {
		return nil, UnknownMatch{Resource: object}
	}
	return templates, nil
}
