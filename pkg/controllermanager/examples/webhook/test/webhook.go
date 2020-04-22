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

package test

import (
	"github.com/gardener/controller-manager-library/pkg/logger"

	"github.com/gardener/controller-manager-library/pkg/controllermanager/webhook"
	"github.com/gardener/controller-manager-library/pkg/controllermanager/webhook/admission"
)

func init() {
	webhook.Configure("test.gardener.cloud").
		Kind(admission.Validating(MyHandlerType)).
		Cluster(webhook.CLUSTER_MAIN).
		Resource("core", "ResourceQuota").
		DefaultedStringOption("message", "yepp", "response message").
		MustRegister()
}

func MyHandlerType(webhook webhook.Interface) (admission.Interface, error) {
	msg, err := webhook.GetStringOption("message")
	if err == nil {
		webhook.Infof("found option message: %s", msg)
	}
	return &MyHandler{message: msg, hook: webhook}, nil
}

type MyHandler struct {
	message string
	admission.DefaultHandler
	hook webhook.Interface
}

var _ admission.Interface = &MyHandler{}

func (this *MyHandler) Handle(logger.LogContext, admission.Request) admission.Response {
	return admission.Allowed(this.message)
	return admission.Denied("aetsch")

}