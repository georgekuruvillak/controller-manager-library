/*
 * Copyright 2019 SAP SE or an SAP affiliate company. All rights reserved.
 * This file is licensed under the Apache Software License, v. 2 except as noted
 * otherwise in the LICENSE file
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 *
 */

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gardener/controller-manager-library/pkg/controllermanager"
	"github.com/gardener/controller-manager-library/pkg/controllermanager/cluster"
	parentcfg "github.com/gardener/controller-manager-library/pkg/controllermanager/config"
	areacfg "github.com/gardener/controller-manager-library/pkg/controllermanager/controller/config"
	"github.com/gardener/controller-manager-library/pkg/ctxutil"
	"github.com/gardener/controller-manager-library/pkg/logger"
	"github.com/gardener/controller-manager-library/pkg/utils"
)

const TYPE = areacfg.OPTION_SOURCE

func init() {
	controllermanager.RegisterExtension(&ExtensionType{DefaultRegistry()})
}

type ExtensionType struct {
	Registry
}

var _ controllermanager.ExtensionType = &ExtensionType{}

func NewExtensionType() *ExtensionType {
	return &ExtensionType{NewRegistry()}
}

func (this *ExtensionType) Name() string {
	return TYPE
}

func (this *ExtensionType) Definition() controllermanager.ExtensionDefinition {
	return NewExtensionDefinition(this.GetDefinitions())
}

////////////////////////////////////////////////////////////////////////////////

type ExtensionDefinition struct {
	definitions Definitions
}

func NewExtensionDefinition(defs Definitions) *ExtensionDefinition {
	return &ExtensionDefinition{
		definitions: defs,
	}
}

func (this *ExtensionDefinition) Name() string {
	return TYPE
}

func (this *ExtensionDefinition) Size() int {
	return this.definitions.Size()
}

func (this *ExtensionDefinition) Names() utils.StringSet {
	return this.definitions.Names()
}

func (this *ExtensionDefinition) Validate() error {
	for n := range this.definitions.Names() {
		for _, r := range this.definitions.Get(n).RequiredControllers() {
			if this.definitions.Get(r) == nil {
				return fmt.Errorf("controller %q requires controller %q, which is not declared", n, r)
			}
		}
	}
	return nil
}

func (this *ExtensionDefinition) ExtendConfig(cfg *parentcfg.Config) {
	my := areacfg.NewConfig()
	this.definitions.ExtendConfig(my)
	cfg.AddSource(areacfg.OPTION_SOURCE, my)
}

func (this *ExtensionDefinition) CreateExtension(logctx logger.LogContext, cm *controllermanager.ControllerManager) (controllermanager.Extension, error) {
	return NewControllerExtension(logctx, this.definitions, cm)
}

////////////////////////////////////////////////////////////////////////////////

type Extension struct {
	SharedAttributes

	manager *controllermanager.ControllerManager

	ctx           context.Context
	config        *areacfg.Config
	definitions   Definitions
	registrations Registrations

	controllers controllers

	plain_groups map[string]StartupGroup
	lease_groups map[string]StartupGroup
	prepared     map[string]*controllermanager.SyncPoint
}

var _ Environment = &Extension{}

func NewControllerExtension(logctx logger.LogContext, defs Definitions, cm *controllermanager.ControllerManager) (*Extension, error) {
	cfg := areacfg.GetConfig(cm.GetConfig())
	ctx := logger.Set(ctxutil.SyncContext(cm.GetContext()), logctx)

	groups := defs.Groups()
	logctx.Infof("configured groups: %s", groups.AllGroups())

	active, err := groups.Activate(strings.Split(cfg.Controllers, ","))
	if err != nil {
		return nil, err
	}

	added := utils.StringSet{}
	for c := range active {
		req, err := defs.GetRequiredControllers(c)
		if err != nil {
			return nil, err
		}
		added.AddSet(req)
	}
	added, _ = active.DiffFrom(added)
	if len(added) > 0 {
		logctx.Infof("controllers implied by activated controllers: %s", added)
		active.AddSet(added)
	}

	registrations, err := defs.Registrations(active.AsArray()...)
	if err != nil {
		return nil, err
	}

	logctx.Infof("activated controllers: %s", active)

	return &Extension{
		SharedAttributes: SharedAttributes{
			LogContext: logctx,
		},
		ctx:           ctx,
		config:        cfg,
		manager:       cm,
		definitions:   defs,
		registrations: registrations,
		prepared:      map[string]*controllermanager.SyncPoint{},

		plain_groups: map[string]StartupGroup{},
		lease_groups: map[string]StartupGroup{},
	}, nil
}

func (this *Extension) Name() string {
	return TYPE
}

func (this *Extension) Namespace() string {
	return this.manager.GetNamespace()
}

func (this *Extension) GetContext() context.Context {
	return this.ctx
}

func (this *Extension) RequiredClusters() (utils.StringSet, error) {
	return this.definitions.DetermineRequestedClusters(this.manager.ClusterDefinitions(), this.registrations.Names())
}

func (this *Extension) GetConfig() *areacfg.Config {
	return this.config
}

func (this *Extension) GetCluster(name string) cluster.Interface {
	return this.manager.GetCluster(name)
}

func (this *Extension) GetClusters() cluster.Clusters {
	return this.manager.GetClusters()
}

func (this *Extension) Start(ctx context.Context) error {
	var err error

	for _, def := range this.registrations {
		lines := strings.Split(def.String(), "\n")
		this.Infof("creating %s", lines[0])
		for _, l := range lines[1:] {
			this.Info(l)
		}
		cmp, err := this.definitions.GetMappingsFor(def.GetName())
		if err != nil {
			return err
		}
		cntr, err := NewController(this, def, cmp)
		if err != nil {
			return err
		}

		if def.RequireLease() {
			this.getLeaseStartupGroup(cntr.GetMainCluster()).Add(cntr)
		} else {
			this.getPlainStartupGroup(cntr.GetMainCluster()).Add(cntr)
		}
		this.controllers = append(this.controllers, cntr)
		this.prepared[cntr.GetName()] = &controllermanager.SyncPoint{}
	}

	this.controllers, err = this.controllers.getOrder(this)
	if err != nil {
		return err
	}

	for _, cntr := range this.controllers {
		err := this.checkController(cntr)
		if err != nil {
			return err
		}
	}

	err = this.startGroups(this.plain_groups, this.lease_groups)
	if err != nil {
		return err
	}

	ctxutil.SyncPointRun(ctx, func() {
		<-this.ctx.Done()
		this.Info("waiting for controllers to shutdown")
		ctxutil.SyncPointWait(this.ctx, 120*time.Second)
		this.Info("all controllers down now")
	})

	return nil
}

// checkController does all the checks that might cause startController to fail
// after the check startController can execute without error
func (this *Extension) checkController(cntr *controller) error {
	return cntr.check()
}

// startController finally starts the controller
// all error conditions MUST also be checked
// in checkController, so after a successful checkController
// startController MUST not return an error.
func (this *Extension) startController(cntr *controller) error {
	for i, a := range cntr.GetDefinition().After() {
		if i == 0 {
			cntr.Infof("observing initialization requirements: %s", utils.Strings(cntr.GetDefinition().After()...))
		}
		after := this.prepared[a]
		if after != nil {
			if !after.IsReached() {
				cntr.Infof("  startup of %q waiting for %q", cntr.GetName(), a)
				if !after.Sync(this.ctx) {
					return fmt.Errorf("setup aborted")
				}
				cntr.Infof("  controller %q is initialized now", a)
			} else {
				cntr.Infof("  controller %q is already initialized", a)
			}
		} else {
			cntr.Infof("  omittimg unused controller %q", a)
		}
	}

	err := cntr.prepare()
	if err != nil {
		return err
	}
	this.prepared[cntr.GetName()].Reach()

	ctxutil.SyncPointRunAndCancelOnExit(this.ctx, cntr.Run)
	return nil
}