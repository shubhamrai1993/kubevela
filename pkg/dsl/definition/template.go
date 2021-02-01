package definition

import (
	"context"
	"encoding/json"
	"fmt"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/build"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oam-dev/kubevela/pkg/dsl/model"
	"github.com/oam-dev/kubevela/pkg/dsl/process"
	"github.com/oam-dev/kubevela/pkg/dsl/task"
	"github.com/oam-dev/kubevela/pkg/oam"
	"github.com/oam-dev/kubevela/pkg/oam/util"
)

const (
	// OutputFieldName is the name of the struct contains the CR data
	OutputFieldName = "output"
	// OutputsFieldName is the name of the struct contains the map[string]CR data
	OutputsFieldName = "outputs"
	// PatchFieldName is the name of the struct contains the patch of CR data
	PatchFieldName = "patch"
	// CustomMessage defines the custom message in definition template
	CustomMessage = "message"
	// HealthCheckPolicy defines the health check policy in definition template
	HealthCheckPolicy = "isHealth"
)

const (
	// AuxiliaryWorkload defines the extra workload obj from a workloadDefinition,
	// e.g. a workload composed by deployment and service, the service will be marked as AuxiliaryWorkload
	AuxiliaryWorkload = "AuxiliaryWorkload"
)

// AbstractEngine defines Definition's Render interface
type AbstractEngine interface {
	Params(params interface{}) AbstractEngine
	Complete(ctx process.Context, abstractTemplate string) error
	HealthCheck(ctx process.Context, cli client.Client, ns string, healthPolicyTemplate string) (bool, error)
	Status(ctx process.Context, cli client.Client, ns string, customStatusTemplate string) (string, error)
}

type def struct {
	name   string
	params interface{}
}

type workloadDef struct {
	def
}

// NewWorkloadAbstractEngine create Workload Definition AbstractEngine
func NewWorkloadAbstractEngine(name string) AbstractEngine {
	return &workloadDef{
		def: def{
			name:   name,
			params: nil,
		},
	}
}

// Params set definition's params
func (wd *workloadDef) Params(params interface{}) AbstractEngine {
	wd.params = params
	return wd
}

// Complete do workload definition's rendering
func (wd *workloadDef) Complete(ctx process.Context, abstractTemplate string) error {
	bi := build.NewContext().NewInstance("", nil)
	if err := bi.AddFile("-", abstractTemplate); err != nil {
		return err
	}
	if wd.params != nil {
		bt, _ := json.Marshal(wd.params)
		if err := bi.AddFile("parameter", fmt.Sprintf("parameter: %s", string(bt))); err != nil {
			return err
		}
	}

	if err := bi.AddFile("-", ctx.BaseContextFile()); err != nil {
		return err
	}
	insts := cue.Build([]*build.Instance{bi})
	for _, inst := range insts {
		if err := inst.Value().Err(); err != nil {
			return errors.WithMessagef(err, "workloadDef %s eval", wd.name)
		}
		output := inst.Lookup(OutputFieldName)
		base, err := model.NewBase(output)
		if err != nil {
			return errors.WithMessagef(err, "workloadDef %s new base", wd.name)
		}
		ctx.SetBase(base)

		// we will support outputs for workload composition, and it will become trait in AppConfig.
		outputs := inst.Lookup(OutputsFieldName)
		st, err := outputs.Struct()
		if err == nil {
			for i := 0; i < st.Len(); i++ {
				fieldInfo := st.Field(i)
				if fieldInfo.IsDefinition || fieldInfo.IsHidden || fieldInfo.IsOptional {
					continue
				}
				other, err := model.NewOther(fieldInfo.Value)
				if err != nil {
					return errors.WithMessagef(err, "parse WorkloadDefinition %s outputs(%s)", wd.name, fieldInfo.Name)
				}
				ctx.PutAuxiliaries(process.Auxiliary{Ins: other, Type: AuxiliaryWorkload, Name: fieldInfo.Name, IsOutputs: true})
			}
		}
	}
	return nil
}

func (wd *workloadDef) getTemplateContext(ctx process.Context, cli client.Reader, ns string) (map[string]interface{}, error) {

	var commonLabels = map[string]string{}
	var root = map[string]interface{}{}
	for k, v := range ctx.BaseContextLabels() {
		root[k] = v
		switch k {
		case "appName":
			commonLabels[oam.LabelAppName] = v
		case "name":
			commonLabels[oam.LabelAppComponent] = v
		}
	}

	base, assists := ctx.Output()
	componentWorkload, err := base.Unstructured()
	if err != nil {
		return nil, err
	}
	// workload main resource will have a unique label("app.oam.dev/resourceType"="WORKLOAD") in per component/app level
	object, err := getResourceFromObj(componentWorkload, cli, ns, util.MergeMapOverrideWithDst(map[string]string{
		oam.LabelOAMResourceType: oam.ResourceTypeWorkload,
	}, commonLabels), "")
	if err != nil {
		return nil, err
	}
	root[OutputFieldName] = object

	for _, assist := range assists {
		if assist.Type != AuxiliaryWorkload {
			continue
		}
		if assist.Name == "" {
			return nil, errors.New("the auxiliary of workload must have a name with format 'outputs.<my-name>'")
		}
		traitRef, err := assist.Ins.Unstructured()
		if err != nil {
			return nil, err
		}
		// AuxiliaryWorkload will have a unique label("trait.oam.dev/resource"="name of outputs") in per component/app level
		object, err := getResourceFromObj(traitRef, cli, ns, util.MergeMapOverrideWithDst(map[string]string{
			oam.TraitTypeLabel: AuxiliaryWorkload,
		}, commonLabels), assist.Name)
		if err != nil {
			return nil, err
		}
		root[OutputsFieldName] = map[string]interface{}{
			assist.Name: object,
		}
	}
	return root, nil
}

// HealthCheck address health check for workload
func (wd *workloadDef) HealthCheck(ctx process.Context, cli client.Client, ns string, healthPolicyTemplate string) (bool, error) {
	if healthPolicyTemplate == "" {
		return true, nil
	}
	templateContext, err := wd.getTemplateContext(ctx, cli, ns)
	if err != nil {
		return false, errors.WithMessage(err, "get template context")
	}
	return checkHealth(templateContext, healthPolicyTemplate)
}

func checkHealth(templateContext map[string]interface{}, healthPolicyTemplate string) (bool, error) {
	bt, err := json.Marshal(templateContext)
	if err != nil {
		return false, errors.WithMessage(err, "json marshal template context")
	}

	var buff = "context: " + string(bt) + "\n" + healthPolicyTemplate
	var r cue.Runtime
	inst, err := r.Compile("-", buff)
	if err != nil {
		return false, errors.WithMessage(err, "compile health template")
	}
	healthy, err := inst.Lookup(HealthCheckPolicy).Bool()
	if err != nil {
		return false, errors.WithMessage(err, "evaluate health status")
	}
	return healthy, nil
}

// Status get workload status by customStatusTemplate
func (wd *workloadDef) Status(ctx process.Context, cli client.Client, ns string, customStatusTemplate string) (string, error) {
	if customStatusTemplate == "" {
		return "", nil
	}
	templateContext, err := wd.getTemplateContext(ctx, cli, ns)
	if err != nil {
		return "", errors.WithMessage(err, "get template context")
	}
	return getStatusMessage(templateContext, customStatusTemplate)
}

func getStatusMessage(templateContext map[string]interface{}, customStatusTemplate string) (string, error) {
	bt, err := json.Marshal(templateContext)
	if err != nil {
		return "", errors.WithMessage(err, "json marshal template context")
	}
	var buff = "context: " + string(bt) + "\n" + customStatusTemplate
	var r cue.Runtime
	inst, err := r.Compile("-", buff)
	if err != nil {
		return "", err
	}
	return inst.Lookup(CustomMessage).String()
}

type traitDef struct {
	def
}

// NewTraitAbstractEngine create Trait Definition AbstractEngine
func NewTraitAbstractEngine(name string) AbstractEngine {
	return &traitDef{
		def: def{
			name: name,
		},
	}
}

// Params set definition's params
func (td *traitDef) Params(params interface{}) AbstractEngine {
	td.params = params
	return td
}

// Complete do trait definition's rendering
func (td *traitDef) Complete(ctx process.Context, abstractTemplate string) error {
	bi := build.NewContext().NewInstance("", nil)
	if err := bi.AddFile("-", abstractTemplate); err != nil {
		return err
	}
	if td.params != nil {
		bt, _ := json.Marshal(td.params)
		if err := bi.AddFile("parameter", fmt.Sprintf("parameter: %s", string(bt))); err != nil {
			return err
		}
	}

	if err := bi.AddFile("f", ctx.BaseContextFile()); err != nil {
		return err
	}
	insts := cue.Build([]*build.Instance{bi})
	for _, inst := range insts {

		if err := inst.Value().Err(); err != nil {
			return errors.WithMessagef(err, "traitDef %s build", td.name)
		}

		processing := inst.Lookup("processing")
		var err error
		if processing.Exists() {
			if inst, err = task.Process(inst); err != nil {
				return errors.WithMessagef(err, "traitDef %s build", td.name)
			}
		}

		output := inst.Lookup(OutputFieldName)
		if output.Exists() {
			other, err := model.NewOther(output)
			if err != nil {
				return errors.WithMessagef(err, "traitDef %s new Assist", td.name)
			}
			ctx.PutAuxiliaries(process.Auxiliary{Ins: other, Type: td.name, IsOutputs: false})
		}

		outputs := inst.Lookup(OutputsFieldName)
		st, err := outputs.Struct()
		if err == nil {
			for i := 0; i < st.Len(); i++ {
				fieldInfo := st.Field(i)
				if fieldInfo.IsDefinition || fieldInfo.IsHidden || fieldInfo.IsOptional {
					continue
				}
				other, err := model.NewOther(fieldInfo.Value)
				if err != nil {
					return errors.WithMessagef(err, "traitDef %s new Assists(%s)", td.name, fieldInfo.Name)
				}
				ctx.PutAuxiliaries(process.Auxiliary{Ins: other, Type: td.name, Name: fieldInfo.Name, IsOutputs: true})
			}
		}

		patcher := inst.Lookup(PatchFieldName)
		if patcher.Exists() {
			base, _ := ctx.Output()
			p, err := model.NewOther(patcher)
			if err != nil {
				return errors.WithMessagef(err, "traitDef %s patcher NewOther", td.name)
			}
			if err := base.Unify(p); err != nil {
				return err
			}
		}
	}
	return nil
}

func (td *traitDef) getTemplateContext(ctx process.Context, cli client.Reader, ns string) (map[string]interface{}, error) {
	var root = map[string]interface{}{}
	var commonLabels = map[string]string{}
	for k, v := range ctx.BaseContextLabels() {
		root[k] = v
		switch k {
		case "appName":
			commonLabels[oam.LabelAppName] = v
		case "name":
			commonLabels[oam.LabelAppComponent] = v
		}
	}
	_, assists := ctx.Output()
	for _, assist := range assists {
		if assist.Type != td.name {
			continue
		}
		traitRef, err := assist.Ins.Unstructured()
		if err != nil {
			return nil, err
		}

		object, err := getResourceFromObj(traitRef, cli, ns, util.MergeMapOverrideWithDst(map[string]string{
			oam.TraitTypeLabel: assist.Type,
		}, commonLabels), assist.Name)
		if err != nil {
			return nil, err
		}
		if assist.IsOutputs {
			root[OutputsFieldName] = map[string]interface{}{
				assist.Name: object,
			}
		} else {
			root[OutputFieldName] = object
		}
	}
	return root, nil
}

// Status get trait status by customStatusTemplate
func (td *traitDef) Status(ctx process.Context, cli client.Client, ns string, customStatusTemplate string) (string, error) {
	if customStatusTemplate == "" {
		return "", nil
	}
	templateContext, err := td.getTemplateContext(ctx, cli, ns)
	if err != nil {
		return "", errors.WithMessage(err, "get template context")
	}
	return getStatusMessage(templateContext, customStatusTemplate)
}

// HealthCheck address health check for trait
func (td *traitDef) HealthCheck(ctx process.Context, cli client.Client, ns string, healthPolicyTemplate string) (bool, error) {
	if healthPolicyTemplate == "" {
		return true, nil
	}
	templateContext, err := td.getTemplateContext(ctx, cli, ns)
	if err != nil {
		return false, errors.WithMessage(err, "get template context")
	}
	return checkHealth(templateContext, healthPolicyTemplate)
}

func getResourceFromObj(obj *unstructured.Unstructured, client client.Reader, namespace string, labels map[string]string, outputsResource string) (map[string]interface{}, error) {
	if outputsResource != "" {
		labels[oam.TraitResource] = outputsResource
	}
	if obj.GetName() != "" {
		u, err := util.GetObjectGivenGVKAndName(context.Background(), client, obj.GroupVersionKind(), namespace, obj.GetName())
		if err != nil {
			return nil, err
		}
		return u.Object, nil
	}
	list, err := util.GetObjectsGivenGVKAndLabels(context.Background(), client, obj.GroupVersionKind(), namespace, labels)
	if err != nil {
		return nil, err
	}
	if len(list.Items) == 1 {
		return list.Items[0].Object, nil
	}
	for _, v := range list.Items {
		if v.GetLabels()[oam.TraitResource] == outputsResource {
			return v.Object, nil
		}
	}
	return nil, errors.Errorf("no resources found gvk(%v) labels(%v)", obj.GroupVersionKind(), labels)
}
