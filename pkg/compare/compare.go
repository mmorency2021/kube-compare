// SPDX-License-Identifier:Apache-2.0

package compare

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	jsonpatch "github.com/evanphx/json-patch"
	"github.com/gosimple/slug"
	"github.com/samber/lo"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/discovery"
	"k8s.io/klog/v2"
	"k8s.io/kubectl/pkg/cmd/diff"
	kcmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
	"k8s.io/utils/exec"
	"sigs.k8s.io/yaml"
)

var (
	compareLong = templates.LongDesc(`
		Compare a known valid reference configuration and a set of specific cluster configuration CRs.
		
		The reference configuration consists of Resource templates. 
		Resource Templates are files that contain Resource definitions and with fixed and optional content. Optional content is represented as Go templates.
		The compare command will match each Resource in the cluster configuration to a Resource Template in the reference 
		configuration. Then, the templated Resource will be injected with the cluster Resource parameters. 
		For each cluster Resource, a diff between the Resource and its matching injected template will be presented
		to the user.
		
		The input cluster configuration may be provided as an "offline" set of CRs or can be pulled from a live cluster.
		
		The Reference also includes a mandatory metadata.yaml file where all the Resource templates should be specified.
		The Resource templates can be divided into components. Each component and Resource template can be set as required,
		resulting in a report to the user in case one of them is missing.
		
		Each Resource definition should be in its own template file. 
		The input to the Go template is the "input cluster configuration" in order to allow expected user variable content
		to be synchronized between cluster CR and reference CR prior to the diff.
		The usage of all Go built-in functions is supported along with the functions in the Sprig library.
		All templates should always be valid YAML after template execution, even when injecting an empty mapping.
		Before using functions that can fail for nil values, always check that the value exists.

		It's possible to pass a user config that contains an option to specify manual matches between cluster resources
		and Resource templates. The matches can be added to the config as pairs of 
		apiVersion_kind_namespace_name: <Template File Name>. For resources that don't have a namespace the matches can 
		be added  as pairs of apiVersion_kind_name: <Template File Name>.

		KUBECTL_EXTERNAL_DIFF environment variable can be used to select your own diff
		command. Users can use external commands with params too, example:
		KUBECTL_EXTERNAL_DIFF="colordiff -N -u"
		
		 By default, the "diff" command available in your path will be run with the "-u"
		(unified diff) and "-N" (treat absent files as empty) options.
		
		 Exit status: 0 No differences were found. 1 Differences were found. >1 kubectl
		or diff failed with an error.
		
		 Note: KUBECTL_EXTERNAL_DIFF, if used, is expected to follow that convention.

		Experimental: This command is under active development and may change without notice.
	`)

	compareExample = templates.Examples(`
		# Compare a known valid reference configuration with a live cluster:
		kubectl cluster-compare -r ./reference/metadata.yaml
		
		# Compare a known valid reference configuration with a local set of CRs:
		kubectl cluster-compare -r ./reference/metadata.yaml -f ./crsdir -R

		# Compare a known valid reference configuration with a live cluster and with a user config:
		kubectl cluster-compare -r ./reference/metadata.yaml -c ./user_config

		# Run a known valid reference configuration with a must-gather output:
		kubectl cluster-compare -r ./reference/metadata.yaml -f "must-gather*/*/cluster-scoped-resources","must-gather*/*/namespaces" -R
	`)
)

const (
	noRefFileWasPassed    = "\"Reference config file is required\""
	refFileNotExistsError = "\"Reference config file doesn't exist\""
	emptyTypes            = "templates don't contain any types (kind) of resources that are supported by the cluster"
	DiffSeparator         = "**********************************\n"
	skipInvalidResources  = "Skipping %s Input contains additional files from supported file extensions" +
		" (json/yaml) that do not contain a valid resource, error: %s.\n In case this file is " +
		"expected to be a valid resource modify it accordingly. "
	DiffsFoundMsg = "there are differences between the cluster CRs and the reference CRs"
)

const (
	Json string = "json"
	Yaml string = "yaml"
)

var OutputFormats = []string{Json, Yaml}

type Options struct {
	CRs                resource.FilenameOptions
	referenceConfig    string
	diffConfigFileName string
	diffAll            bool
	verboseOutput      bool
	ShowManagedFields  bool
	OutputFormat       string

	builder        *resource.Builder
	correlator     *MultiCorrelator
	metricsTracker *MetricsTracker
	templates      []*ReferenceTemplate
	local          bool
	types          []string
	ref            Reference
	userConfig     UserConfig
	Concurrency    int

	diff *diff.DiffProgram
	genericiooptions.IOStreams
}

func NewCmd(f kcmdutil.Factory, streams genericiooptions.IOStreams) *cobra.Command {
	options := NewOptions(streams)
	example := compareExample
	if strings.HasPrefix(filepath.Base(os.Args[0]), "oc-") {
		example = strings.ReplaceAll(compareExample, "kubectl", "oc")
	} else if !strings.HasPrefix(filepath.Base(os.Args[0]), "kubectl-") {
		example = strings.ReplaceAll(compareExample, "kubectl ", "")
	}

	cmd := &cobra.Command{
		Use:                   "compare -r <Reference File>",
		DisableFlagsInUseLine: true,
		Short:                 i18n.T("Compare a reference configuration and a set of cluster configuration CRs."),
		Long:                  compareLong,
		Example:               example,
		Run: func(cmd *cobra.Command, args []string) {
			kcmdutil.CheckDiffErr(options.Complete(f, cmd, args))
			// `kubectl cluster-compare` propagates the error code from
			// `kubectl diff` that propagates the error code from
			// diff or `KUBECTL_EXTERNAL_DIFF`. Also, we
			// don't want to print an error if diff returns
			// error code 1, which simply means that changes
			// were found. We also don't want kubectl to
			// return 1 if there was a problem.
			if err := options.Run(); err != nil {
				if exitErr := diffError(err); exitErr != nil {
					kcmdutil.CheckErr(kcmdutil.ErrExit)
				}
				kcmdutil.CheckDiffErr(err)
			}
		},
	}

	// Flag errors exit with code 1, however according to the diff
	// command it means changes were found.
	// Thus, it should return status code greater than 1.
	cmd.SetFlagErrorFunc(func(command *cobra.Command, err error) error {
		kcmdutil.CheckDiffErr(kcmdutil.UsageErrorf(cmd, err.Error()))
		return nil
	})
	cmd.Flags().IntVar(&options.Concurrency, "concurrency", 4,
		"Number of objects to process in parallel when diffing against the live version. Larger number = faster,"+
			" but more memory, I/O and CPU over that shorter period of time.")
	kcmdutil.AddFilenameOptionFlags(cmd, &options.CRs, "contains the configuration to diff")
	cmd.Flags().StringVarP(&options.diffConfigFileName, "diff-config", "c", "", "Path to the user config file")
	cmd.Flags().StringVarP(&options.referenceConfig, "reference", "r", "", "Path to reference config file.")
	cmd.Flags().BoolVar(&options.ShowManagedFields, "show-managed-fields", options.ShowManagedFields, "If true, include managed fields in the diff.")
	cmd.Flags().BoolVarP(&options.diffAll, "all-resources", "A", options.diffAll,
		"If present, In live mode will try to match all resources that are from the types mentioned in the reference. "+
			"In local mode will try to match all resources passed to the command")
	cmd.Flags().BoolVarP(&options.verboseOutput, "verbose", "v", options.verboseOutput, "Increases the verbosity of the tool")

	cmd.Flags().StringVarP(&options.OutputFormat, "output", "o", "", fmt.Sprintf(`Output format. One of: (%s)`, strings.Join(OutputFormats, ", ")))
	kcmdutil.CheckErr(cmd.RegisterFlagCompletionFunc(
		"output",
		func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			var comps []string
			for _, format := range OutputFormats {
				if strings.HasPrefix(format, toComplete) {
					comps = append(comps, format)
				}
			}
			return comps, cobra.ShellCompDirectiveNoFileComp
		},
	))

	return cmd
}

func NewOptions(ioStreams genericiooptions.IOStreams) *Options {
	return &Options{
		IOStreams: ioStreams,
		diff: &diff.DiffProgram{
			Exec:      exec.New(),
			IOStreams: ioStreams,
		},
	}
}

// DiffError returns the ExitError if the status code is less than 1,
// nil otherwise.
func diffError(err error) exec.ExitError {
	var execErr exec.ExitError
	if ok := errors.As(err, &execErr); ok && execErr.ExitStatus() <= 1 {
		return execErr
	}
	return nil
}

func GetRefFS(refConfig string) (fs.FS, error) {
	referenceDir := filepath.Dir(refConfig)
	if isURL(refConfig) {
		// filepath.Dir removes one / from http://
		referenceDir = strings.Replace(referenceDir, "/", "//", 1)
		return HTTPFS{baseURL: referenceDir, httpGet: httpgetImpl}, nil
	}
	rootPath, err := filepath.Abs(referenceDir)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path: %w", err)
	}
	return os.DirFS(rootPath), nil
}
func (o *Options) Complete(f kcmdutil.Factory, cmd *cobra.Command, args []string) error {
	var err error
	o.builder = f.NewBuilder()

	if o.referenceConfig == "" {
		return kcmdutil.UsageErrorf(cmd, noRefFileWasPassed)
	}
	if _, err := os.Stat(o.referenceConfig); os.IsNotExist(err) && !isURL(o.referenceConfig) {
		return fmt.Errorf(refFileNotExistsError)
	}

	cfs, err := GetRefFS(o.referenceConfig)
	if err != nil {
		return err
	}

	referenceFileName := filepath.Base(o.referenceConfig)
	o.ref, err = GetReference(cfs, referenceFileName)
	if err != nil {
		return err
	}

	if o.diffConfigFileName != "" {
		o.userConfig, err = parseDiffConfig(o.diffConfigFileName)
		if err != nil {
			return err
		}
	}
	o.templates, err = ParseTemplates(o.ref.GetTemplates(), o.ref.TemplateFunctionFiles, cfs, &o.ref)
	if err != nil {
		return err
	}

	err = o.setupCorrelators()
	if err != nil {
		return err
	}

	if len(args) != 0 {
		return kcmdutil.UsageErrorf(cmd, "Unexpected args: %v", args)
	}
	err = o.CRs.RequireFilenameOrKustomize()

	if err == nil {
		o.local = true
		o.types = []string{}
		return nil
	}

	return o.setLiveSearchTypes(f)
}

// setupCorrelators initializes a chain of correlators based on the provided options.
// The correlation chain consists of base correlators wrapped with decorator correlators.
// This function configures the following base correlators:
//  1. ExactMatchCorrelator - Matches CRs based on pairs specifying, for each cluster CR, its matching template.
//     The pairs are read from the diff config and provided to the correlator.
//  2. GroupCorrelator - Matches CRs based on groups of fields that are similar in cluster resources and templates.
//
// The base correlators are combined using a MultiCorrelator, which attempts to match a template for each base correlator
// in the specified sequence.
func (o *Options) setupCorrelators() error {
	var correlators []Correlator
	if len(o.userConfig.CorrelationSettings.ManualCorrelation.CorrelationPairs) > 0 {
		manualCorrelator, err := NewExactMatchCorrelator(o.userConfig.CorrelationSettings.ManualCorrelation.CorrelationPairs, o.templates)
		if err != nil {
			return err
		}
		correlators = append(correlators, manualCorrelator)
	}

	// These fields are used by the GroupCorrelator who attempts to match templates based on the following priority order:
	// apiVersion_name_namespace_kind. If no single match is found, it proceeds to trying matching by apiVersion_name_kind,
	// then namespace_kind, and finally kind alone.
	//
	// For instance, consider a template resource with fixed apiVersion, name, and kind, but a templated namespace. The
	// correlator will potentially match this template based on its fixed fields: apiVersion_name_kind.
	var fieldGroups = [][][]string{
		{{"apiVersion"}, {"metadata", "name"}, {"metadata", "namespace"}, {"kind"}},
		{{"apiVersion"}, {"metadata", "namespace"}, {"kind"}},
		{{"metadata", "name"}, {"metadata", "namespace"}, {"kind"}},
		{{"apiVersion"}, {"metadata", "name"}, {"kind"}},
		{{"metadata", "name"}, {"kind"}},
		{{"metadata", "namespace"}, {"kind"}},
		{{"apiVersion"}, {"kind"}},
		{{"kind"}},
	}
	groupCorrelator, err := NewGroupCorrelator(fieldGroups, o.templates)
	if err != nil {
		return err
	}

	correlators = append(correlators, groupCorrelator)

	o.correlator = NewMultiCorrelator(correlators)
	o.metricsTracker = NewMetricsTracker()
	return nil
}

// setLiveSearchTypes creates a set of resources types to search the live cluster for in order to retrieve cluster resources.
// The types are gathered from the templates included in the reference. The set of types is filtered, so it will include only
// types supported by the live cluster in order to not raise errors by the visitor. In a case the reference includes types that
// are not supported by the user a warning will be created.
func (o *Options) setLiveSearchTypes(f kcmdutil.Factory) error {
	kindSet := make(map[string][]*ReferenceTemplate)
	for _, t := range o.templates {
		kindSet[t.metadata.GetKind()] = append(kindSet[t.metadata.GetKind()], t)
	}

	c, err := f.ToDiscoveryClient()
	if err != nil {
		return fmt.Errorf("failed to create discovery client: %w", err)
	}
	SupportedTypes, err := getSupportedResourceTypes(c)
	if err != nil {
		return err
	}
	var notSupportedTypes []string
	o.types, notSupportedTypes = findAllRequestedSupportedTypes(SupportedTypes, kindSet)
	if len(o.types) == 0 {
		return errors.New(emptyTypes)
	}
	if len(notSupportedTypes) > 0 {
		sort.Strings(notSupportedTypes)
		klog.Warningf("Reference Contains Templates With Types (kind) Not Supported By Cluster: %s", strings.Join(notSupportedTypes, ", "))
	}

	return nil
}

// getSupportedResourceTypes retrieves a set of resource types that are supported by the cluster. For each supported
// resource type it will specify a list of groups where it exists.
func getSupportedResourceTypes(client discovery.CachedDiscoveryInterface) (map[string][]string, error) {
	resources := make(map[string][]string)
	lists, err := client.ServerPreferredResources()
	if err != nil {
		return resources, fmt.Errorf("failed to get clusters resource types: %w", err)
	}
	for _, list := range lists {
		if len(list.APIResources) != 0 {
			for _, res := range list.APIResources {
				resources[res.Kind] = append(resources[res.Kind], res.Group)
			}
		}
	}
	return resources, nil

}

// findAllRequestedSupportedTypes divides the requested types in to two groups: supported types and unsupported types based on if they are specified as supported.
// The list of supported types will include the types in the form of {kind}.{group}.
func findAllRequestedSupportedTypes(supportedTypesWithGroups map[string][]string, requestedTypes map[string][]*ReferenceTemplate) ([]string, []string) {
	var typesIncludingGroup []string
	var notSupportedTypes []string
	for kind := range requestedTypes {
		if _, ok := supportedTypesWithGroups[kind]; ok {
			for _, group := range supportedTypesWithGroups[kind] {
				typesIncludingGroup = append(typesIncludingGroup, strings.Join([]string{kind, group}, "."))
			}
		} else {
			notSupportedTypes = append(notSupportedTypes, kind)
		}
	}
	return typesIncludingGroup, notSupportedTypes
}

func extractPath(str string, pathIndex int) string {
	if split := strings.Split(str, " "); len(split) >= pathIndex {
		return split[pathIndex]
	}
	return "Unknown Path"
}

func getBestMatchByLines(templates []*ReferenceTemplate, cr *unstructured.Unstructured, o *Options) (*ReferenceTemplate, *bytes.Buffer, error) {
	var bestTemp *ReferenceTemplate
	minDiffNum := math.MaxInt
	var minDiffOutput *bytes.Buffer
	for _, temp := range templates {
		diffOutput, err := diffAgainstTemplate(temp, cr, o)
		if err != nil {
			return nil, minDiffOutput, err
		}
		minDiffNum = min(bytes.Count(diffOutput.Bytes(), []byte("\n")), minDiffNum)
		if minDiffNum == bytes.Count(diffOutput.Bytes(), []byte("\n")) {
			bestTemp = temp
			minDiffOutput = diffOutput
		}
	}
	return bestTemp, minDiffOutput, nil
}

func diffAgainstTemplate(temp *ReferenceTemplate, clusterCR *unstructured.Unstructured, o *Options) (*bytes.Buffer, error) {
	localRef, err := temp.Exec(clusterCR.Object)
	if err != nil {
		return nil, err
	}
	obj := InfoObject{
		injectedObjFromTemplate: localRef,
		clusterObj:              clusterCR,
		FieldsToOmit:            temp.FieldsToOmit(o.ref.FieldsToOmit),
		allowMerge:              temp.Config.AllowMerge,
	}

	differ, err := diff.NewDiffer("MERGED", "LIVE")
	diffOutput := new(bytes.Buffer)
	if err != nil {
		return diffOutput, fmt.Errorf("failed to create diff instance: %w", err)
	}
	defer differ.TearDown()

	err = differ.Diff(obj, diff.Printer{}, o.ShowManagedFields)
	if err != nil {
		return diffOutput, fmt.Errorf("error occurered during diff: %w", err)
	}
	err = differ.Run(&diff.DiffProgram{Exec: exec.New(), IOStreams: genericiooptions.IOStreams{In: o.IOStreams.In, Out: diffOutput, ErrOut: o.IOStreams.ErrOut}})

	// If the diff tool runs without issues and detects differences at this level of the code, we would like to report that there are no issues
	var exitErr exec.ExitError
	if ok := errors.As(err, &exitErr); ok && exitErr.ExitStatus() <= 1 {
		return diffOutput, nil
	}
	if err != nil {
		return diffOutput, fmt.Errorf("diff exited with non-zero code: %w", err)
	}
	return diffOutput, nil
}

// Run uses the factory to parse file arguments (in case of local mode) or gather all cluster resources matching
// templates types. For each Resource it finds the matching Resource template and
// injects, compares, and runs against differ.
func (o *Options) Run() error {
	diffs := make([]DiffSum, 0)
	numDiffCRs := 0

	r := o.builder.
		Unstructured().
		VisitorConcurrency(o.Concurrency).
		AllNamespaces(true).
		LocalParam(o.local).
		FilenameParam(false, &o.CRs).
		ResourceTypes(o.types...).
		SelectAllParam(!o.local).
		ContinueOnError().
		Flatten().
		Do()
	if err := r.Err(); err != nil {
		return fmt.Errorf("failed to collect resources: %w", err)
	}
	r.IgnoreErrors(func(err error) bool {
		if strings.Contains(err.Error(), "Object 'Kind' is missing") {
			klog.Warningf(skipInvalidResources, extractPath(err.Error(), 3), "'Kind' is missing")
			return true
		}
		if strings.Contains(err.Error(), "error parsing") {
			klog.Warningf(skipInvalidResources, extractPath(err.Error(), 2), err.Error()[strings.LastIndex(err.Error(), ":"):])
			return true
		}
		return containOnly(err, []error{UnknownMatch{}, MergeError{}})
	})

	err := r.Visit(func(info *resource.Info, _ error) error { // ignoring previous errors
		clusterCRMapping, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(info.Object)
		clusterCR := &unstructured.Unstructured{Object: clusterCRMapping}

		temps, err := o.correlator.Match(clusterCR)
		if err != nil && (!containOnly(err, []error{UnknownMatch{}}) || o.diffAll) {
			o.metricsTracker.addUNMatch(clusterCR)
		}
		if err != nil {
			return err
		}

		temp, diffOutput, err := getBestMatchByLines(temps, clusterCR, o)

		if err != nil {
			o.metricsTracker.addUNMatch(clusterCR)
			return err
		}

		o.metricsTracker.addMatch(temp)

		if diffOutput.Len() > 0 {
			numDiffCRs += 1
		}

		diffs = append(diffs, DiffSum{DiffOutput: diffOutput.String(), CorrelatedTemplate: temp.Name(), CRName: apiKindNamespaceName(clusterCR)})
		return err
	})
	if err != nil {
		return fmt.Errorf("error occurred while trying to process resources: %w", err)
	}

	sum := newSummary(&o.ref, o.metricsTracker, numDiffCRs)

	_, err = Output{Summary: sum, Diffs: &diffs}.Print(o.OutputFormat, o.Out, o.verboseOutput)
	if err != nil {
		return err
	}

	// We will return exit code 1 in case there are differences between the reference CRs and cluster CRs.
	// The differences can be differences found in specific CRs or the absence of CRs from the cluster.
	if numDiffCRs != 0 || sum.NumMissing != 0 {
		return exec.CodeExitError{Err: errors.New(DiffsFoundMsg), Code: 1}
	}
	return nil
}

// InfoObject matches the diff.Object interface, it contains the objects that shall be compared.
type InfoObject struct {
	injectedObjFromTemplate *unstructured.Unstructured
	clusterObj              *unstructured.Unstructured
	FieldsToOmit            []*ManifestPath
	allowMerge              bool
}

// Live Returns the cluster version of the object
func (obj InfoObject) Live() runtime.Object {
	omitFields(obj.clusterObj.Object, obj.FieldsToOmit)
	return obj.clusterObj
}

type MergeError struct {
	obj *InfoObject
	err error
}

func (e MergeError) Error() string {
	return fmt.Sprintf("failed to properly merge the manifests for %s some diff may be incorrect: %s", e.obj.Name(), e.err)
}

// Merged Returns the Injected Reference Version of the Resource
func (obj InfoObject) Merged() (runtime.Object, error) {
	var err error
	if obj.allowMerge {
		obj.injectedObjFromTemplate, err = MergeManifests(obj.injectedObjFromTemplate, obj.clusterObj)
		if err != nil {
			return obj.injectedObjFromTemplate, &MergeError{obj: &obj, err: err}
		}
	}
	omitFields(obj.injectedObjFromTemplate.Object, obj.FieldsToOmit)
	return obj.injectedObjFromTemplate, err
}

func findFieldPaths(object map[string]any, fields []*ManifestPath) [][]string {
	result := make([][]string, 0)
	for _, f := range fields {
		if !f.IsPrefix {
			result = append(result, f.parts)
		} else {
			start := f.parts[:len(f.parts)-1]
			prefix := f.parts[len(f.parts)-1]

			val, _, _ := unstructured.NestedFieldNoCopy(object, start...)
			if mapping, ok := val.(map[string]any); ok {
				for key := range mapping {
					if strings.HasPrefix(key, prefix) {
						newPath := append([]string{}, start...)
						newPath = append(newPath, key)
						result = append(result, newPath)
					}
				}
			}
		}
	}

	return result
}

func omitFields(object map[string]any, fields []*ManifestPath) {
	fieldPaths := findFieldPaths(object, fields)

	for _, field := range fieldPaths {
		unstructured.RemoveNestedField(object, field...)
		for i := 0; i <= len(field); i++ {
			val, _, _ := unstructured.NestedFieldNoCopy(object, field[:len(field)-i]...)
			if mapping, ok := val.(map[string]any); ok && len(mapping) == 0 {
				unstructured.RemoveNestedField(object, field[:len(field)-i]...)
			}
		}
	}
}

// MergeManifests will return an attempt to update the localRef with the clusterCR. In the case of an error it will return an unmodified localRef.
func MergeManifests(localRef, clusterCR *unstructured.Unstructured) (updateLocalRef *unstructured.Unstructured, err error) {
	localRefData, err := json.Marshal(localRef)
	if err != nil {
		return localRef, fmt.Errorf("failed to marshal reference CR: %w", err)
	}

	clusterCRData, err := json.Marshal(clusterCR.Object)
	if err != nil {
		return localRef, fmt.Errorf("failed to marshal cluster CR: %w", err)
	}

	localRefUpdatedData, err := jsonpatch.MergePatch(clusterCRData, localRefData)
	if err != nil {
		return localRef, fmt.Errorf("failed to merge cluster and reference CRs: %w", err)
	}

	localRefUpdatedObj := make(map[string]any)
	err = json.Unmarshal(localRefUpdatedData, &localRefUpdatedObj)
	if err != nil {
		return localRef, fmt.Errorf("failed to unmarshal updated manifest: %w", err)
	}

	return &unstructured.Unstructured{Object: localRefUpdatedObj}, nil
}

func (obj InfoObject) Name() string {
	return slug.Make(apiKindNamespaceName(obj.clusterObj))
}

// DiffSum Contains the diff output and correlation info of a specific CR
type DiffSum struct {
	DiffOutput         string `json:"DiffOutput"`
	CorrelatedTemplate string `json:"CorrelatedTemplate"`
	CRName             string `json:"CRName"`
}

func (s DiffSum) String() string {
	t := `
Cluster CR: {{ .CRName }}
Reference File: {{ .CorrelatedTemplate }}
{{- if ne (len  .DiffOutput) 0 }}
Diff Output: {{ .DiffOutput }}
{{- else }}
Diff Output: None
{{ end }}
`
	var buf bytes.Buffer
	tmpl, _ := template.New("DiffSummary").Parse(t)
	_ = tmpl.Execute(&buf, s)
	return strings.TrimSpace(buf.String())
}

func (s DiffSum) HasDiff() bool {
	return s.DiffOutput != ""
}

// Summary Contains all info included in the Summary output of the compare command
type Summary struct {
	RequiredCRS  map[string]map[string][]string `json:"RequiredCRS"`
	NumMissing   int                            `json:"NumMissing"`
	UnmatchedCRS []string                       `json:"UnmatchedCRS"`
	NumDiffCRs   int                            `json:"NumDiffCRs"`
	TotalCRs     int                            `json:"TotalCRs"`
}

func newSummary(reference *Reference, c *MetricsTracker, numDiffCRs int) *Summary {
	s := Summary{NumDiffCRs: numDiffCRs}
	s.RequiredCRS, s.NumMissing = reference.getMissingCRs(c.MatchedTemplatesNames)
	s.TotalCRs = len(c.MatchedTemplatesNames)
	s.UnmatchedCRS = lo.Map(c.UnMatchedCRs, func(r *unstructured.Unstructured, i int) string {
		return apiKindNamespaceName(r)
	})
	return &s
}

func (s Summary) String() string {
	t := `
Summary
CRs with diffs: {{ .NumDiffCRs }}/{{ .TotalCRs }}
{{- if ne (len  .RequiredCRS) 0 }}
CRs in reference missing from the cluster: {{.NumMissing}}
{{ toYaml .RequiredCRS}}
{{- else}}
No CRs are missing from the cluster
{{- end }}
{{- if ne (len  .UnmatchedCRS) 0 }}
Cluster CRs unmatched to reference CRs: {{len  .UnmatchedCRS}}
{{ toYaml .UnmatchedCRS}}
{{- else}}
No CRs are unmatched to reference CRs
{{- end }}
`
	var buf bytes.Buffer
	tmpl, _ := template.New("Summary").Funcs(template.FuncMap{"toYaml": toYAML}).Parse(t)
	_ = tmpl.Execute(&buf, s)
	return strings.TrimSpace(buf.String())
}

// Output Contains the complete output of the command
type Output struct {
	Summary *Summary   `json:"Summary"`
	Diffs   *[]DiffSum `json:"Diffs"`
}

func (o Output) String(showEmptyDiffs bool) string {
	sort.Slice(*o.Diffs, func(i, j int) bool {
		return (*o.Diffs)[i].CorrelatedTemplate+(*o.Diffs)[i].CRName < (*o.Diffs)[j].CorrelatedTemplate+(*o.Diffs)[j].CRName
	})

	diffParts := []string{}

	for _, diffSum := range *o.Diffs {
		if showEmptyDiffs || diffSum.HasDiff() {
			diffParts = append(diffParts, fmt.Sprintln(diffSum.String()))
		}
	}

	var str string
	if len(diffParts) > 0 {
		partsStr := strings.Join(diffParts, fmt.Sprintf("\n%s\n", DiffSeparator))
		str = fmt.Sprintf("%s\n%s\n%s\n", DiffSeparator, partsStr, DiffSeparator)
	}

	return fmt.Sprintf("%s%s\n", str, o.Summary.String())
}

func (o Output) Print(format string, out io.Writer, showEmptyDiffs bool) (int, error) {
	var (
		content []byte
		err     error
	)
	switch format {
	case Json:
		content, err = json.Marshal(o)
		if err != nil {
			return 0, fmt.Errorf("failed to marshal output to json: %w", err)
		}
		content = append(content, []byte("\n")...)

	case Yaml:
		content, err = yaml.Marshal(o)
		if err != nil {
			return 0, fmt.Errorf("failed to marshal output to yaml: %w", err)
		}
	default:
		content = []byte(o.String(showEmptyDiffs))
	}
	n, err := out.Write(content)
	if err != nil {
		return n, fmt.Errorf("error occurred when writing output: %w", err)
	}
	return n, nil
}
