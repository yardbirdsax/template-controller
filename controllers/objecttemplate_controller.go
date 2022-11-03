/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"github.com/hashicorp/go-multierror"
	"github.com/kluctl/go-jinja2"
	templatesv1alpha1 "github.com/kluctl/template-controller/api/v1alpha1"
	"github.com/ohler55/ojg/jp"
	"io"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	"strings"
	"sync"
)

const forMatrixObjectKey = "spec.matrix.object.ref"

// ObjectTemplateReconciler reconciles a ObjectTemplate object
type ObjectTemplateReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	FieldManager string

	controller   controller.Controller
	watchedKinds map[schema.GroupVersionKind]bool
	mutex        sync.Mutex
}

//+kubebuilder:rbac:groups=templates.kluctl.io,resources=objecttemplates,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=templates.kluctl.io,resources=objecttemplates/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=templates.kluctl.io,resources=objecttemplates/finalizers,verbs=update
//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// Reconcile a resource
func (r *ObjectTemplateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rt templatesv1alpha1.ObjectTemplate
	err := r.Get(ctx, req.NamespacedName, &rt)
	if err != nil {
		return ctrl.Result{}, err
	}

	for _, me := range rt.Spec.Matrix {
		if me.Object != nil {
			gvk, err := me.Object.Ref.GroupVersionKind()
			if err != nil {
				return ctrl.Result{}, err
			}
			err = r.addWatchForKind(gvk)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	patch := client.MergeFrom(rt.DeepCopy())
	err = r.doReconcile(ctx, &rt)
	if err != nil {
		c := metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: rt.GetGeneration(),
			Reason:             "Error",
			Message:            err.Error(),
		}
		apimeta.SetStatusCondition(&rt.Status.Conditions, c)
	} else {
		c := metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			ObservedGeneration: rt.GetGeneration(),
			Reason:             "Success",
			Message:            "Success",
		}
		apimeta.SetStatusCondition(&rt.Status.Conditions, c)
	}
	err = r.Status().Patch(ctx, &rt, patch)
	if err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{
		RequeueAfter: rt.Spec.Interval.Duration,
	}, nil
}

func (r *ObjectTemplateReconciler) multiplyMatrix(matrix []map[string]any, key string, newElems []any) []map[string]any {
	var newMatrix []map[string]any

	for _, m := range matrix {
		for _, e := range newElems {
			newME := map[string]any{}
			for k, v := range m {
				newME[k] = v
			}
			newME[key] = e
			newMatrix = append(newMatrix, newME)
		}
	}

	return newMatrix
}

func (r *ObjectTemplateReconciler) buildMatrixObjectElements(ctx context.Context, rt *templatesv1alpha1.ObjectTemplate, me *templatesv1alpha1.MatrixEntryObject) ([]any, error) {
	gvk, err := me.Ref.GroupVersionKind()
	if err != nil {
		return nil, err
	}
	namespace := rt.Namespace
	if me.Ref.Namespace != "" {
		namespace = me.Ref.Namespace
	}

	var o unstructured.Unstructured
	o.SetGroupVersionKind(gvk)

	err = r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: me.Ref.Name}, &o)
	if err != nil {
		return nil, err
	}

	jp, err := jp.ParseString(me.JsonPath)
	if err != nil {
		return nil, err
	}

	var elems []any
	for _, x := range jp.Get(o.Object) {
		if me.ExpandLists {
			if l, ok := x.([]any); ok {
				elems = append(elems, l...)
			} else {
				elems = append(elems, x)
			}
		} else {
			elems = append(elems, x)
		}
	}
	return elems, nil
}

func (r *ObjectTemplateReconciler) buildMatrixEntries(ctx context.Context, rt *templatesv1alpha1.ObjectTemplate) ([]map[string]any, error) {
	var err error
	var matrixEntries []map[string]any
	matrixEntries = append(matrixEntries, map[string]any{})

	for _, me := range rt.Spec.Matrix {
		var elems []any
		if me.Object != nil {
			elems, err = r.buildMatrixObjectElements(ctx, rt, me.Object)
			if err != nil {
				return nil, err
			}
		} else if me.List != nil {
			for _, le := range me.List {
				var e any
				err := yaml.Unmarshal(le.Raw, &e)
				if err != nil {
					return nil, err
				}
				elems = append(elems, e)
			}
		} else {
			return nil, fmt.Errorf("missing matrix value")
		}

		matrixEntries = r.multiplyMatrix(matrixEntries, me.Name, elems)
	}
	return matrixEntries, nil
}

func (r *ObjectTemplateReconciler) doReconcile(ctx context.Context, rt *templatesv1alpha1.ObjectTemplate) error {
	baseVars, err := r.buildBaseVars(ctx, rt)
	if err != nil {
		return err
	}

	j2, err := NewJinja2()
	if err != nil {
		return err
	}
	defer j2.Close()

	var allResources []*unstructured.Unstructured
	var errs *multierror.Error
	var wg sync.WaitGroup
	var mutex sync.Mutex

	matrixEntries, err := r.buildMatrixEntries(ctx, rt)
	if err != nil {
		return err
	}

	wg.Add(len(matrixEntries))
	for _, matrix := range matrixEntries {
		matrix := matrix
		go func() {
			defer wg.Done()
			vars := runtime.DeepCopyJSON(baseVars)
			MergeMap(vars, map[string]interface{}{
				"matrix": matrix,
			})

			resources, err := r.renderTemplates(ctx, j2, rt, vars)
			if err != nil {
				errs = multierror.Append(errs, err)
				return
			}

			mutex.Lock()
			defer mutex.Unlock()
			allResources = append(allResources, resources...)
		}()
	}
	wg.Wait()
	if errs != nil {
		return errs
	}

	for _, x := range allResources {
		if x.GetNamespace() == "" {
			x.SetNamespace(rt.Namespace)
		}
	}

	toDelete := make(map[templatesv1alpha1.ObjectRef]templatesv1alpha1.ObjectRef)
	for _, n := range rt.Status.AppliedResources {
		gvk, err := n.Ref.GroupVersionKind()
		if err != nil {
			return err
		}
		ref := n.Ref
		ref.APIVersion = gvk.Group
		toDelete[ref] = n.Ref
	}

	rt.Status.AppliedResources = nil

	wg.Add(len(allResources))
	for _, resource := range allResources {
		resource := resource

		ref := templatesv1alpha1.ObjectRefFromObject(resource)
		gvk, err := ref.GroupVersionKind()
		if err != nil {
			return err
		}

		ari := templatesv1alpha1.AppliedResourceInfo{
			Ref:     ref,
			Success: true,
		}

		ref.APIVersion = gvk.Group
		delete(toDelete, ref)

		go func() {
			defer wg.Done()
			err := r.applyTemplate(ctx, rt, resource)
			if err != nil {
				ari.Success = false
				ari.Error = err.Error()
				errs = multierror.Append(errs, err)
			}
		}()

		rt.Status.AppliedResources = append(rt.Status.AppliedResources, ari)
	}
	wg.Wait()

	wg.Add(len(toDelete))
	for _, ref := range toDelete {
		gvk, err := ref.GroupVersionKind()
		if err != nil {
			return err
		}
		m := metav1.PartialObjectMetadata{}
		m.SetGroupVersionKind(gvk)
		m.SetNamespace(ref.Namespace)
		m.SetName(ref.Name)

		go func() {
			defer wg.Done()
			err := r.Delete(ctx, &m)
			if err != nil {
				if !errors.IsNotFound(err) {
					errs = multierror.Append(errs, err)
				}
			}
		}()
	}
	wg.Wait()

	return errs.ErrorOrNil()
}

func (r *ObjectTemplateReconciler) applyTemplate(ctx context.Context, rt *templatesv1alpha1.ObjectTemplate, rendered *unstructured.Unstructured) error {
	err := r.Client.Patch(ctx, rendered, client.Apply, client.FieldOwner(r.FieldManager))
	if err != nil {
		return err
	}
	return nil
}

func (r *ObjectTemplateReconciler) renderTemplates(ctx context.Context, j2 *jinja2.Jinja2, rt *templatesv1alpha1.ObjectTemplate, vars map[string]any) ([]*unstructured.Unstructured, error) {
	var ret []*unstructured.Unstructured
	for _, t := range rt.Spec.Templates {
		if t.Object != nil {
			x := t.Object.DeepCopy()
			_, err := j2.RenderStruct(x, jinja2.WithGlobals(vars))
			if err != nil {
				return nil, err
			}
			ret = append(ret, x)
		} else if t.Raw != nil {
			r, err := j2.RenderString(*t.Raw, jinja2.WithGlobals(vars))
			if err != nil {
				return nil, err
			}
			d := yaml.NewYAMLToJSONDecoder(strings.NewReader(r))
			for {
				var u unstructured.Unstructured
				err = d.Decode(&u)
				if err != nil {
					if err == io.EOF {
						break
					}
					return nil, err
				}
				ret = append(ret, &u)
			}
		} else {
			return nil, fmt.Errorf("no template specified")
		}
	}
	return ret, nil
}

func (r *ObjectTemplateReconciler) buildBaseVars(ctx context.Context, rt *templatesv1alpha1.ObjectTemplate) (map[string]any, error) {
	vars := map[string]any{}

	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(rt)
	if err != nil {
		return nil, err
	}

	vars["objectTemplate"] = u
	return vars, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ObjectTemplateReconciler) SetupWithManager(mgr ctrl.Manager, concurrent int) error {
	r.watchedKinds = map[schema.GroupVersionKind]bool{}

	// Index the ObjectHandler by the objects they are for.
	if err := mgr.GetCache().IndexField(context.TODO(), &templatesv1alpha1.ObjectTemplate{}, forMatrixObjectKey,
		func(object client.Object) []string {
			o := object.(*templatesv1alpha1.ObjectTemplate)
			var ret []string
			for _, me := range o.Spec.Matrix {
				if me.Object != nil {
					ret = append(ret, BuildRefIndexValue(me.Object.Ref, o.GetNamespace()))
				}
			}
			return ret
		}); err != nil {
		return fmt.Errorf("failed setting index fields: %w", err)
	}

	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&templatesv1alpha1.ObjectTemplate{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: concurrent,
		}).
		Build(r)
	if err != nil {
		return err
	}
	r.controller = c

	return nil
}

func (r *ObjectTemplateReconciler) addWatchForKind(gvk schema.GroupVersionKind) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if x, ok := r.watchedKinds[gvk]; ok && x {
		return nil
	}

	var dummy unstructured.Unstructured
	dummy.SetGroupVersionKind(gvk)

	err := r.controller.Watch(&source.Kind{Type: &dummy}, handler.EnqueueRequestsFromMapFunc(func(object client.Object) []reconcile.Request {
		var list templatesv1alpha1.ObjectTemplateList
		err := r.List(context.Background(), &list, client.MatchingFields{
			forMatrixObjectKey: BuildObjectIndexValue(object),
		})
		if err != nil {
			return nil
		}
		var reqs []reconcile.Request
		for _, x := range list.Items {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: x.Namespace,
					Name:      x.Name,
				},
			})
		}
		return reqs
	}))
	if err != nil {
		return err
	}

	r.watchedKinds[gvk] = true
	return nil
}
