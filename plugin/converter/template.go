// Copyright 2019 Drone IO, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !oss
// +build !oss

package converter

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"path/filepath"
	"regexp"
	templating "text/template"

	"github.com/drone/funcmap"

	"github.com/drone/drone/core"
	"github.com/drone/drone/plugin/converter/jsonnet"
	"github.com/drone/drone/plugin/converter/starlark"

	"gopkg.in/yaml.v2"
)

var (
	// templateFileRE regex to verifying kind is template.
	templateFileRE              = regexp.MustCompilePOSIX("^kind:[[:space:]]+template[[:space:]]?+$")
	errTemplateNotFound         = errors.New("template converter: template name given not found")
	errTemplateSyntaxErrors     = errors.New("template converter: there is a problem with the yaml file provided")
	errTemplateExtensionInvalid = errors.New("template extension invalid. must be yaml, starlark or jsonnet")
)

func Template(templateStore core.TemplateStore, stepLimit uint64, sizeLimit uint64) core.ConvertService {
	return &templatePlugin{
		templateStore: templateStore,
		stepLimit:     stepLimit,
		sizeLimit:     sizeLimit,
	}
}

type templatePlugin struct {
	templateStore core.TemplateStore
	stepLimit     uint64
	sizeLimit     uint64
}

func (p *templatePlugin) Convert(ctx context.Context, req *core.ConvertArgs) (*core.Config, error) {
	// check type is yaml
	configExt := filepath.Ext(req.Repo.Config)

	if configExt != ".yml" && configExt != ".yaml" {
		return nil, nil
	}

	// check kind is template
	if templateFileRE.MatchString(req.Config.Data) == false {
		return nil, nil
	}
	// map to templateArgs

	buf := new(bytes.Buffer)
	offset := 0
	for {
		templateReader := bytes.NewBuffer([]byte(req.Config.Data)[offset:])
		decoder := yaml.NewDecoder(templateReader)
		var tmp map[string]interface{}
		if err := decoder.Decode(&tmp); err != nil {
			if err == io.EOF {
				break
			}
			return nil, errTemplateSyntaxErrors
		}
		buf.WriteString("\n")

		kind, ok := tmp["kind"]
		if !ok {
			return nil, errTemplateSyntaxErrors
		}

		switch kind {
		case "template":
			templateArgs := core.TemplateArgs{
				Kind: "template",
				Load: tmp["load"].(string),
			}
			data := make(map[string]interface{})
			for k, v := range tmp["data"].(map[interface{}]interface{}) {
				data[k.(string)] = v
			}
			templateArgs.Data = data
			// get template from db
			template, err := p.templateStore.FindName(ctx, templateArgs.Load, req.Repo.Namespace)
			if err == sql.ErrNoRows {
				return nil, errTemplateNotFound
			}
			if err != nil {
				return nil, err
			}

			// parse template
			res, err := p.parseTemplate(req, template, templateArgs)
			if err != nil {
				return nil, err
			}
			writeBytes, err := buf.WriteString(res)
			if err != nil {
				return nil, err
			}
			offset += writeBytes
		case "pipeline":
			writeBytes, err := buf.Write([]byte(req.Config.Data)[offset:])
			if err != nil {
				return nil, err
			}
			offset += writeBytes
		default:
			return nil, errTemplateSyntaxErrors
		}
	}

	return &core.Config{Data: buf.String()}, nil
}

func (p *templatePlugin) parseTemplate(req *core.ConvertArgs, template *core.Template, templateArgs core.TemplateArgs) (string, error) {
	switch filepath.Ext(templateArgs.Load) {
	case ".yml", ".yaml":
		return parseYaml(req, template, templateArgs)
	case ".star", ".starlark", ".script":
		return parseStarlark(req, template, templateArgs, p.stepLimit, p.sizeLimit)
	case ".jsonnet":
		return parseJsonnet(req, template, templateArgs)
	default:
		return "", errTemplateExtensionInvalid
	}
}

func parseYaml(req *core.ConvertArgs, template *core.Template, templateArgs core.TemplateArgs) (string, error) {
	data := map[string]interface{}{
		"build": toBuild(req.Build),
		"repo":  toRepo(req.Repo),
		"input": templateArgs.Data,
	}
	tmpl, err := templating.New(template.Name).Funcs(funcmap.SafeFuncs).Parse(template.Data)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	err = tmpl.Execute(&out, data)
	if err != nil {
		return "", err
	}
	return out.String(), nil
}

func parseJsonnet(req *core.ConvertArgs, template *core.Template, templateArgs core.TemplateArgs) (string, error) {
	file, err := jsonnet.Parse(req, nil, 0, template, templateArgs.Data)
	if err != nil {
		return "", err
	}
	return file, nil
}

func parseStarlark(req *core.ConvertArgs, template *core.Template, templateArgs core.TemplateArgs, stepLimit uint64, sizeLimit uint64) (string, error) {
	file, err := starlark.Parse(req, template, templateArgs.Data, stepLimit, sizeLimit)
	if err != nil {
		return "", err
	}
	return file, nil
}
