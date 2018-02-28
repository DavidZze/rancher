package workload

import (
	"errors"
	"fmt"
	"strings"

	"github.com/docker/distribution/reference"
	"github.com/rancher/norman/api/access"
	"github.com/rancher/norman/httperror"
	"github.com/rancher/norman/types"
	"github.com/rancher/norman/types/convert"
	"github.com/rancher/norman/types/values"
	projectschema "github.com/rancher/types/apis/project.cattle.io/v3/schema"
	"github.com/rancher/types/client/project/v3"
	projectclient "github.com/rancher/types/client/project/v3"
	"github.com/rancher/types/config"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
)

const (
	SelectorLabel = "workload.user.cattle.io/workloadselector"
)

type AggregateStore struct {
	Stores          map[string]types.Store
	Schemas         map[string]*types.Schema
	FieldToSchemaID map[string]string
}

func NewAggregateStore(schemas ...*types.Schema) *AggregateStore {
	a := &AggregateStore{
		Stores:          map[string]types.Store{},
		Schemas:         map[string]*types.Schema{},
		FieldToSchemaID: map[string]string{},
	}

	for _, schema := range schemas {
		a.Schemas[strings.ToLower(schema.ID)] = schema
		a.Stores[strings.ToLower(schema.ID)] = schema.Store
		fieldKey := fmt.Sprintf("%sConfig", schema.ID)
		a.FieldToSchemaID[fieldKey] = strings.ToLower(schema.ID)
	}

	return a
}

func (a *AggregateStore) Context() types.StorageContext {
	return config.UserStorageContext
}

func (a *AggregateStore) ByID(apiContext *types.APIContext, schema *types.Schema, id string) (map[string]interface{}, error) {
	store, schemaType, err := a.getStore(id)
	if err != nil {
		return nil, err
	}
	_, shortID := splitTypeAndID(id)
	return store.ByID(apiContext, a.Schemas[schemaType], shortID)
}

func (a *AggregateStore) Watch(apiContext *types.APIContext, schema *types.Schema, opt *types.QueryOptions) (chan map[string]interface{}, error) {
	readerGroup, ctx := errgroup.WithContext(apiContext.Request.Context())
	apiContext.Request = apiContext.Request.WithContext(ctx)

	events := make(chan map[string]interface{})
	for _, schema := range a.Schemas {
		streamStore(readerGroup, apiContext, schema, opt, events)
	}

	go func() {
		readerGroup.Wait()
		close(events)
	}()
	return events, nil
}

func (a *AggregateStore) List(apiContext *types.APIContext, schema *types.Schema, opt *types.QueryOptions) ([]map[string]interface{}, error) {
	items := make(chan map[string]interface{})
	g, ctx := errgroup.WithContext(apiContext.Request.Context())
	submit := func(schema *types.Schema, store types.Store) {
		g.Go(func() error {
			data, err := store.List(apiContext, schema, opt)
			if err != nil {
				return err
			}
			for _, item := range data {
				select {
				case items <- item:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		})
	}

	for typeName, store := range a.Stores {
		submit(a.Schemas[typeName], store)
	}

	go func() {
		g.Wait()
		close(items)
	}()

	var result []map[string]interface{}
	for item := range items {
		result = append(result, item)
	}

	return result, g.Wait()
}

func (a *AggregateStore) Create(apiContext *types.APIContext, schema *types.Schema, data map[string]interface{}) (map[string]interface{}, error) {
	// deployment is default if otherwise is not specified
	kind := client.DeploymentType
	toSchema := a.Schemas[kind]
	toStore := a.Stores[kind]
	for field, schemaID := range a.FieldToSchemaID {
		if val, ok := data[field]; ok && val != nil {
			toSchema = a.Schemas[schemaID]
			toStore = a.Stores[schemaID]
			break
		}
	}

	setSelector(toSchema.ID, data)
	setWorkloadSpecificDefaults(toSchema.ID, data)
	setSecrets(apiContext, data)

	return toStore.Create(apiContext, toSchema, data)
}

func setSelector(schemaID string, data map[string]interface{}) {
	setSelector := false
	isJob := strings.EqualFold(schemaID, "job") || strings.EqualFold(schemaID, "cronJob")
	if convert.IsEmpty(data["selector"]) && !isJob {
		setSelector = true
	}
	if setSelector {
		workloadID := resolveWorkloadID(schemaID, data)
		// set selector
		data["selector"] = map[string]interface{}{
			"matchLabels": map[string]interface{}{
				SelectorLabel: workloadID,
			},
		}

		// set workload labels
		workloadLabels := convert.ToMapInterface(data["workloadLabels"])
		if workloadLabels == nil {
			workloadLabels = make(map[string]interface{})
		}
		workloadLabels[SelectorLabel] = workloadID
		data["workloadLabels"] = workloadLabels

		// set labels
		labels := convert.ToMapInterface(data["labels"])
		if labels == nil {
			labels = make(map[string]interface{})
		}
		labels[SelectorLabel] = workloadID
		data["labels"] = labels
	}
}

func setWorkloadSpecificDefaults(schemaID string, data map[string]interface{}) {
	if strings.EqualFold(schemaID, "job") || strings.EqualFold(schemaID, "cronJob") {
		// job has different defaults
		if _, ok := data["restartPolicy"]; !ok {
			logrus.Info("Setting restart policy")
			data["restartPolicy"] = "OnFailure"
		}
	}
}

func store(registries map[string]projectclient.RegistryCredential, domainToCreds map[string][]corev1.LocalObjectReference, name string) {
	for registry := range registries {
		secretRef := corev1.LocalObjectReference{Name: name}
		if _, ok := domainToCreds[registry]; ok {
			domainToCreds[registry] = append(domainToCreds[registry], secretRef)
		} else {
			domainToCreds[registry] = []corev1.LocalObjectReference{secretRef}
		}
	}
}

func getCreds(apiContext *types.APIContext) map[string][]corev1.LocalObjectReference {
	domainToCreds := make(map[string][]corev1.LocalObjectReference)
	var namespacedCreds []projectclient.NamespacedDockerCredential
	if err := access.List(apiContext, &projectschema.Version, "namespacedDockerCredential", &types.QueryOptions{}, &namespacedCreds); err == nil {
		for _, cred := range namespacedCreds {
			store(cred.Registries, domainToCreds, cred.Name)
		}
	}
	var creds []projectclient.DockerCredential
	if err := access.List(apiContext, &projectschema.Version, "dockerCredential", &types.QueryOptions{}, &creds); err == nil {
		for _, cred := range creds {
			store(cred.Registries, domainToCreds, cred.Name)
		}
	}
	return domainToCreds
}

func setSecrets(apiContext *types.APIContext, data map[string]interface{}) {
	if val, _ := values.GetValue(data, "imagePullSecrets"); val != nil {
		return
	}
	if containers, _ := values.GetSlice(data, "containers"); len(containers) > 0 {
		imagePullSecrets, _ := data["imagePullSecrets"].([]corev1.LocalObjectReference)
		domainToCreds := getCreds(apiContext)
		for _, container := range containers {
			if image := convert.ToString(container["image"]); image != "" {
				domain := getDomain(image)
				if secrets, ok := domainToCreds[domain]; ok {
					imagePullSecrets = append(imagePullSecrets, secrets...)
				}
			}
		}
		if imagePullSecrets != nil {
			values.PutValue(data, imagePullSecrets, "imagePullSecrets")
		}
	}
}

func resolveWorkloadID(schemaID string, data map[string]interface{}) string {
	return fmt.Sprintf("%s-%s-%s", schemaID, data["namespaceId"], data["name"])
}

func (a *AggregateStore) Update(apiContext *types.APIContext, schema *types.Schema, data map[string]interface{}, id string) (map[string]interface{}, error) {
	store, schemaType, err := a.getStore(id)
	if err != nil {
		return nil, err
	}
	_, shortID := splitTypeAndID(id)
	return store.Update(apiContext, a.Schemas[schemaType], data, shortID)
}

func (a *AggregateStore) Delete(apiContext *types.APIContext, schema *types.Schema, id string) (map[string]interface{}, error) {
	store, schemaType, err := a.getStore(id)
	if err != nil {
		return nil, err
	}
	_, shortID := splitTypeAndID(id)
	return store.Delete(apiContext, a.Schemas[schemaType], shortID)
}

func (a *AggregateStore) getStore(id string) (types.Store, string, error) {
	typeName, _ := splitTypeAndID(id)
	store, ok := a.Stores[typeName]
	if !ok {
		return nil, "", httperror.NewAPIError(httperror.NotFound, "failed to find type "+typeName)
	}
	return store, typeName, nil
}

func streamStore(eg *errgroup.Group, apiContext *types.APIContext, schema *types.Schema, opt *types.QueryOptions, result chan map[string]interface{}) {
	eg.Go(func() error {
		events, err := schema.Store.Watch(apiContext, schema, opt)
		if err != nil || events == nil {
			if err != nil {
				logrus.Errorf("failed on subscribe %s: %v", schema.ID, err)
			}
			return err
		}

		logrus.Debugf("watching %s", schema.ID)

		for e := range events {
			result <- e
		}

		return errors.New("disconnect")
	})
}

func splitTypeAndID(id string) (string, string) {
	parts := strings.SplitN(id, ":", 2)
	if len(parts) < 2 {
		// Must conform
		return "", ""
	}
	return parts[0], parts[1]
}

func getDomain(image string) string {
	var repo string
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		logrus.Debug(err)
		return repo
	}
	domain := reference.Domain(named)
	if domain == "docker.io" {
		return "index.docker.io"
	}
	return domain
}