package v1

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	sq "github.com/Masterminds/squirrel"
	"github.com/argoproj/argo/pkg/apis/workflow/v1alpha1"
	argojson "github.com/argoproj/pkg/json"
	"github.com/ghodss/yaml"
	"github.com/onepanelio/core/pkg/util"
	"github.com/onepanelio/core/pkg/util/label"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (c *Client) createWorkflowTemplate(namespace string, workflowTemplate *WorkflowTemplate) (*WorkflowTemplate, error) {
	uid, err := workflowTemplate.GenerateUID()
	if err != nil {
		return nil, err
	}

	tx, err := c.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	versionUnix := time.Now().Unix()

	err = sb.Insert("workflow_templates").
		SetMap(sq.Eq{
			"uid":       uid,
			"name":      workflowTemplate.Name,
			"namespace": namespace,
		}).
		Suffix("RETURNING id").
		RunWith(tx).
		QueryRow().Scan(&workflowTemplate.ID)
	if err != nil {
		return nil, err
	}

	_, err = sb.Insert("workflow_template_versions").
		SetMap(sq.Eq{
			"workflow_template_id": workflowTemplate.ID,
			"version":              versionUnix,
			"is_latest":            true,
			"manifest":             workflowTemplate.Manifest,
		}).
		RunWith(tx).
		Exec()
	if err != nil {
		return nil, err
	}

	argoWft, err := createArgoWorkflowTemplate(workflowTemplate, versionUnix)
	argoWft, err = c.ArgoprojV1alpha1().WorkflowTemplates(namespace).Create(argoWft)
	if err != nil {
		return nil, err
	}

	if err = tx.Commit(); err != nil {
		if err := c.ArgoprojV1alpha1().WorkflowTemplates(namespace).Delete(argoWft.Name, &v1.DeleteOptions{}); err != nil {
			log.Printf("Unable to delete argo workflow template")
		}
		return nil, err
	}

	workflowTemplate.Version = versionUnix

	return workflowTemplate, nil
}

func (c *Client) workflowTemplatesSelectBuilder(namespace string) sq.SelectBuilder {
	sb := sb.Select("wt.id", "wt.created_at", "wt.uid", "wt.name", "wt.is_archived").
		From("workflow_templates wt").
		Where(sq.Eq{
			"wt.namespace": namespace,
		})

	return sb
}

func (c *Client) workflowTemplatesVersionSelectBuilder(namespace string) sq.SelectBuilder {
	sb := sb.Select("wtv.id", "wtv.version", "wtv.is_latest", "wtv.manifest", "wtv.created_at").
		From("workflow_template_versions wtv").
		Join("workflow_templates wt ON wt.id = wtv.workflow_template_id").
		Where(sq.Eq{
			"wt.namespace": namespace,
		})

	return sb
}

func (c *Client) getWorkflowTemplate(namespace, uid string, version int64) (workflowTemplate *WorkflowTemplate, err error) {
	workflowTemplate = &WorkflowTemplate{
		WorkflowExecutionStatisticReport: &WorkflowExecutionStatisticReport{},
	}

	sb := c.workflowTemplatesSelectBuilder(namespace).
		Column("wtv.manifest").
		Join("workflow_template_versions wtv ON wt.id = wtv.workflow_template_id").
		Where(sq.Eq{"wt.uid": uid})

	if version == 0 {
		sb = sb.Where(sq.Eq{"wtv.is_latest": true})
	} else {
		sb = sb.Where(sq.Eq{"wtv.version": version})
	}

	query, args, err := sb.ToSql()
	if err != nil {
		return
	}

	if err = c.DB.Get(workflowTemplate, query, args...); err == sql.ErrNoRows {
		err = nil
		workflowTemplate = nil
	}

	if workflowTemplate == nil {
		return workflowTemplate, nil
	}

	versionAsString := "latest"
	if version != 0 {
		versionAsString = fmt.Sprintf("%v", version)
	}

	argoWft, err := c.getArgoWorkflowTemplate(namespace, uid, versionAsString)
	if err != nil {
		return nil, err
	}
	workflowTemplate.ArgoWorkflowTemplate = argoWft

	templateVersion, err := strconv.ParseInt(argoWft.Labels[label.Version], 10, 64)
	if err != nil {
		return nil, err
	}

	workflowTemplate.Version = templateVersion

	return workflowTemplate, nil
}

func (c *Client) getWorkflowTemplateByName(namespace, name string, version int64) (workflowTemplate *WorkflowTemplate, err error) {
	workflowTemplate = &WorkflowTemplate{}

	sb := c.workflowTemplatesSelectBuilder(namespace).Where(sq.Eq{"wt.name": name}).
		Columns("wtv.manifest").
		OrderBy("wtv.version desc").
		Limit(1)
	if version != 0 {
		sb = sb.Where(sq.Eq{"wtv.version": version})
	}
	query, args, err := sb.ToSql()
	if err != nil {
		return
	}

	if err = c.DB.Get(workflowTemplate, query, args...); err == sql.ErrNoRows {
		err = nil
		workflowTemplate = nil
	}

	return
}

func (c *Client) listWorkflowTemplateVersions(namespace, uid string) (workflowTemplateVersions []*WorkflowTemplate, err error) {
	template, err := c.GetWorkflowTemplate(namespace, uid, 0)
	if err != nil {
		return nil, err
	}

	argoTemplates, err := c.listArgoWorkflowTemplates(namespace, uid)
	if err != nil {
		return nil, err
	}

	dbVersions, err := c.listDbWorkflowTemplateVersions(namespace, uid)
	if err != nil {
		return nil, err
	}

	mapByVersion := make(map[int64]*WorkflowTemplateVersion)
	for _, dbVersion := range dbVersions {
		mapByVersion[dbVersion.Version] = dbVersion
	}

	for _, argoTemplate := range *argoTemplates {
		version, versionErr := strconv.ParseInt(argoTemplate.Labels[label.Version], 10, 64)
		if versionErr != nil {
			return nil, versionErr
		}

		isLatest := false
		if _, ok := argoTemplate.Labels[label.VersionLatest]; ok {
			isLatest = true
		}

		dbVersion := mapByVersion[version]

		labels := label.FilterByPrefix(label.TagPrefix, argoTemplate.Labels)

		newItem := WorkflowTemplate{
			ID:         template.ID,
			CreatedAt:  argoTemplate.CreationTimestamp.Time,
			UID:        template.UID,
			Name:       template.Name,
			Manifest:   dbVersion.Manifest,
			Version:    version,
			IsLatest:   isLatest,
			IsArchived: template.IsArchived,
			Labels:     labels,
		}

		workflowTemplateVersions = append(workflowTemplateVersions, &newItem)
	}

	return
}

func (c *Client) listWorkflowTemplates(namespace string) (workflowTemplateVersions []*WorkflowTemplate, err error) {
	workflowTemplateVersions = []*WorkflowTemplate{}

	query, args, err := c.workflowTemplatesSelectBuilder(namespace).
		Column("COUNT(wtv.*) versions").
		Options("DISTINCT ON (wt.id)").
		Join("workflow_template_versions wtv ON wtv.workflow_template_id = wt.id").
		GroupBy("wt.id", "wt.created_at", "wt.uid", "wt.name", "wt.is_archived").
		Where(sq.Eq{
			"wt.is_archived": false,
		}).
		OrderBy("wt.id desc").ToSql()
	if err != nil {
		return
	}

	err = c.DB.Select(&workflowTemplateVersions, query, args...)

	return
}

func (c *Client) archiveWorkflowTemplate(namespace, uid string) (bool, error) {
	query, args, err := sb.Update("workflow_templates").
		Set("is_archived", true).
		Where(sq.Eq{
			"uid":       uid,
			"namespace": namespace,
		}).
		ToSql()

	if err != nil {
		return false, err
	}

	if _, err := c.DB.Exec(query, args...); err != nil {
		return false, err
	}

	return true, nil
}

func (c *Client) CreateWorkflowTemplate(namespace string, workflowTemplate *WorkflowTemplate) (*WorkflowTemplate, error) {
	// validate workflow template
	finalBytes, err := workflowTemplate.WrapSpec()
	if err != nil {
		return nil, util.NewUserError(codes.InvalidArgument, err.Error())
	}

	if err := c.ValidateWorkflowExecution(namespace, finalBytes); err != nil {
		log.WithFields(log.Fields{
			"Namespace":        namespace,
			"WorkflowTemplate": workflowTemplate,
			"Error":            err.Error(),
		}).Error("Workflow could not be validated.")
		return nil, util.NewUserError(codes.InvalidArgument, err.Error())
	}

	workflowTemplate, err = c.createWorkflowTemplate(namespace, workflowTemplate)
	if err != nil {
		log.WithFields(log.Fields{
			"Namespace":        namespace,
			"WorkflowTemplate": workflowTemplate,
			"Error":            err.Error(),
		}).Error("Could not create workflow template.")
		return nil, util.NewUserErrorWrap(err, "Workflow template")
	}

	return workflowTemplate, nil
}

func (c *Client) CreateWorkflowTemplateVersion(namespace string, workflowTemplate *WorkflowTemplate) (*WorkflowTemplate, error) {
	// validate workflow template
	finalBytes, err := workflowTemplate.WrapSpec()
	if err != nil {
		return nil, util.NewUserError(codes.InvalidArgument, err.Error())
	}

	if err := c.ValidateWorkflowExecution(namespace, finalBytes); err != nil {
		log.WithFields(log.Fields{
			"Namespace":        namespace,
			"WorkflowTemplate": workflowTemplate,
			"Error":            err.Error(),
		}).Error("Workflow could not be validated.")
		return nil, util.NewUserError(codes.InvalidArgument, err.Error())
	}

	versionUnix := time.Now().Unix()

	tx, err := c.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	wftSb := c.workflowTemplatesSelectBuilder(namespace).Where(sq.Eq{"wt.uid": workflowTemplate.UID})
	query, args, err := wftSb.ToSql()
	if err != nil {
		return nil, err
	}
	workflowTemplateDb := &WorkflowTemplate{}
	if err = c.DB.Get(workflowTemplateDb, query, args...); err != nil {
		return nil, err
	}

	_, err = sb.Update("workflow_template_versions").
		Set("is_latest", false).
		Where(sq.Eq{
			"workflow_template_id": workflowTemplateDb.ID,
		}).
		RunWith(tx).
		Exec()
	if err != nil {
		return nil, err
	}

	_, err = sb.Insert("workflow_template_versions").
		SetMap(sq.Eq{
			"workflow_template_id": workflowTemplateDb.ID,
			"version":              versionUnix,
			"is_latest":            true,
			"manifest":             workflowTemplate.Manifest,
		}).
		RunWith(tx).
		Exec()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	latest, err := c.getArgoWorkflowTemplate(namespace, workflowTemplate.UID, "latest")
	if err != nil {
		log.WithFields(log.Fields{
			"Namespace":        namespace,
			"WorkflowTemplate": workflowTemplate,
			"Error":            err.Error(),
		}).Error("Could not get latest argo workflow template")

		return nil, err
	}

	delete(latest.Labels, label.VersionLatest)

	if _, err := c.ArgoprojV1alpha1().WorkflowTemplates(namespace).Update(latest); err != nil {
		return nil, err
	}

	updatedTemplate, err := createArgoWorkflowTemplate(workflowTemplate, versionUnix)
	if err != nil {
		return nil, err
	}

	updatedTemplate.TypeMeta = v1.TypeMeta{}
	updatedTemplate.ObjectMeta.ResourceVersion = ""
	updatedTemplate.ObjectMeta.SetSelfLink("")

	parametersMap, err := workflowTemplate.GetParametersKeyString()
	if err != nil {
		return nil, err
	}

	if updatedTemplate.Annotations == nil {
		updatedTemplate.Annotations = make(map[string]string)
	}
	for key, value := range parametersMap {
		updatedTemplate.Annotations[key] = value
	}

	if _, err := c.ArgoprojV1alpha1().WorkflowTemplates(namespace).Create(updatedTemplate); err != nil {
		return nil, err
	}

	return workflowTemplate, nil
}

// If version is 0, it returns the latest.
func (c *Client) GetWorkflowTemplate(namespace, uid string, version int64) (workflowTemplate *WorkflowTemplate, err error) {
	workflowTemplate, err = c.getWorkflowTemplate(namespace, uid, version)
	if err != nil {
		log.WithFields(log.Fields{
			"Namespace":        namespace,
			"WorkflowTemplate": workflowTemplate,
			"Error":            err.Error(),
		}).Error("Get Workflow Template failed.")
		return nil, util.NewUserError(codes.Unknown, "Unknown error.")
	}
	if workflowTemplate == nil {
		return nil, util.NewUserError(codes.NotFound, "Workflow template not found.")
	}

	return
}

func (c *Client) GetWorkflowTemplateByName(namespace, name string, version int64) (workflowTemplate *WorkflowTemplate, err error) {
	workflowTemplate, err = c.getWorkflowTemplateByName(namespace, name, version)
	if err != nil {
		log.WithFields(log.Fields{
			"Namespace":        namespace,
			"WorkflowTemplate": workflowTemplate,
			"Error":            err.Error(),
		}).Error("Get Workflow Template failed.")
		return nil, util.NewUserError(codes.Unknown, "Unknown error.")
	}
	if workflowTemplate == nil {
		return nil, util.NewUserError(codes.NotFound, "Workflow template not found.")
	}

	return
}

func (c *Client) ListWorkflowTemplateVersions(namespace, uid string) (workflowTemplateVersions []*WorkflowTemplate, err error) {
	workflowTemplateVersions, err = c.listWorkflowTemplateVersions(namespace, uid)
	if err != nil {
		log.WithFields(log.Fields{
			"Namespace": namespace,
			"UID":       uid,
			"Error":     err.Error(),
		}).Error("Workflow template versions not found.")
		return nil, util.NewUserError(codes.NotFound, "Workflow template versions not found.")
	}

	return
}

func (c *Client) ListWorkflowTemplates(namespace string) (workflowTemplateVersions []*WorkflowTemplate, err error) {
	workflowTemplateVersions, err = c.listWorkflowTemplates(namespace)
	if err != nil {
		log.WithFields(log.Fields{
			"Namespace": namespace,
			"Error":     err.Error(),
		}).Error("Workflow templates not found.")
		return nil, util.NewUserError(codes.NotFound, "Workflow templates not found.")
	}

	err = c.GetWorkflowExecutionStatisticsForTemplates(workflowTemplateVersions...)
	if err != nil {
		log.WithFields(log.Fields{
			"Namespace": namespace,
			"Error":     err.Error(),
		}).Error("Unable to get Workflow Execution Statistic for Templates.")
		return nil, util.NewUserError(codes.NotFound, "Unable to get Workflow Execution Statistic for Templates.")
	}
	for _, workflowTemplate := range workflowTemplateVersions {
		labels, err := c.GetWorkflowTemplateLabels(namespace, workflowTemplate.UID, label.TagPrefix, workflowTemplate.Version)
		if err != nil {
			log.WithFields(log.Fields{
				"Namespace": namespace,
				"Error":     err.Error(),
			}).Error("Unable to get GetWorkflowTemplateLabels for Templates.")
			continue
		}

		workflowTemplate.Labels = labels
	}

	return
}

func (c *Client) ArchiveWorkflowTemplate(namespace, uid string) (archived bool, err error) {
	workflowTemplate, err := c.getWorkflowTemplate(namespace, uid, 0)
	if err != nil {
		log.WithFields(log.Fields{
			"Namespace": namespace,
			"UID":       uid,
			"Error":     err.Error(),
		}).Error("Get Workflow Template failed.")
		return false, util.NewUserError(codes.Unknown, "Unable to archive workflow template.")
	}
	if workflowTemplate == nil {
		return false, util.NewUserError(codes.NotFound, "Workflow template not found.")
	}

	archived, err = c.archiveWorkflowTemplate(namespace, uid)
	if !archived || err != nil {
		if err != nil {
			log.WithFields(log.Fields{
				"Namespace": namespace,
				"UID":       uid,
				"Error":     err.Error(),
			}).Error("Archive Workflow Template failed.")
		}
		return false, util.NewUserError(codes.Unknown, "Unable to archive workflow template.")
	}

	return
}

func createArgoWorkflowTemplate(workflowTemplate *WorkflowTemplate, version int64) (*v1alpha1.WorkflowTemplate, error) {
	var argoWft *v1alpha1.WorkflowTemplate
	var jsonOpts []argojson.JSONOpt
	jsonOpts = append(jsonOpts, argojson.DisallowUnknownFields)

	finalBytes, err := workflowTemplate.WrapSpec()
	if err != nil {
		return nil, err
	}

	err = yaml.Unmarshal(finalBytes, &argoWft)
	if err != nil {
		return nil, err
	}

	re, _ := regexp.Compile(`[^a-zA-Z0-9-]{1,}`)
	worfklowTemplateName := strings.ToLower(re.ReplaceAllString(workflowTemplate.Name, `-`))

	argoWft.Name = fmt.Sprintf("%v-v%v", worfklowTemplateName, version)

	labels := map[string]string{
		label.WorkflowTemplate:    worfklowTemplateName,
		label.WorkflowTemplateUid: workflowTemplate.UID,
		label.Version:             fmt.Sprintf("%v", version),
		label.VersionLatest:       "true",
	}

	label.MergeLabelsPrefix(labels, workflowTemplate.Labels, label.TagPrefix)
	argoWft.Labels = labels

	return argoWft, nil
}

// version "latest" will get the latest version.
func (c *Client) getArgoWorkflowTemplate(namespace, workflowTemplateUid, version string) (*v1alpha1.WorkflowTemplate, error) {
	labelSelect := fmt.Sprintf("%v=%v", label.WorkflowTemplateUid, workflowTemplateUid)
	if version == "latest" {
		labelSelect += "," + label.VersionLatest + "=true"
	} else {
		labelSelect += fmt.Sprintf(",%v=%v", label.Version, version)
	}

	workflowTemplates, err := c.ArgoprojV1alpha1().WorkflowTemplates(namespace).List(v1.ListOptions{
		LabelSelector: labelSelect,
	})
	if err != nil {
		return nil, err
	}

	templates := workflowTemplates.Items
	if templates.Len() == 0 {
		return nil, errors.New("not found")
	}

	if templates.Len() > 1 {
		return nil, errors.New("not unique result")
	}

	return &templates[0], nil
}

func (c *Client) listArgoWorkflowTemplates(namespace, workflowTemplateUid string) (*[]v1alpha1.WorkflowTemplate, error) {
	labelSelect := fmt.Sprintf("%v=%v", label.WorkflowTemplateUid, workflowTemplateUid)
	workflowTemplates, err := c.ArgoprojV1alpha1().WorkflowTemplates(namespace).List(v1.ListOptions{
		LabelSelector: labelSelect,
	})
	if err != nil {
		return nil, err
	}

	templates := []v1alpha1.WorkflowTemplate(workflowTemplates.Items)

	return &templates, nil
}

func (c *Client) listDbWorkflowTemplateVersions(namespace, workflowTemplateUid string) ([]*WorkflowTemplateVersion, error) {
	versions := make([]*WorkflowTemplateVersion, 0)

	sb := c.workflowTemplatesVersionSelectBuilder(namespace)
	sb.Where(sq.Eq{"wt.uid": workflowTemplateUid})

	sql, args, err := sb.ToSql()
	if err != nil {
		return versions, err
	}

	if err := c.DB.Select(&versions, sql, args...); err != nil {
		return versions, err
	}

	return versions, nil
}

// prefix is the label prefix.
// e.g. prefix/my-label-key: my-label-value
// if version is 0, latest is used.
func (c *Client) GetWorkflowTemplateLabels(namespace, name, prefix string, version int64) (labels map[string]string, err error) {
	versionAsString := "latest"
	if version != 0 {
		versionAsString = fmt.Sprintf("%v", version)
	}

	wf, err := c.getArgoWorkflowTemplate(namespace, name, versionAsString)
	if err != nil {
		log.WithFields(log.Fields{
			"Namespace": namespace,
			"Name":      name,
			"Error":     err.Error(),
		}).Error("Workflow Template not found.")
		return nil, util.NewUserError(codes.NotFound, "Workflow Template not found.")
	}

	labels = label.FilterByPrefix(prefix, wf.Labels)
	labels = label.RemovePrefix(prefix, labels)

	return
}
