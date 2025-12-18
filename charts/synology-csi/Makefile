HELM_OPTS ?= $(OPTS)
NAMESPACE ?= synology-csi
RELEASE ?= $(NAMESPACE)
HELM_NAMESPACE ?= $(NAMESPACE)
HELM_RELEASE ?= $(RELEASE)

.PHONY: up upgrade
up upgrade:
	helm upgrade $(HELM_RELEASE) . --create-namespace --install --namespace $(HELM_NAMESPACE) $(HELM_OPTS)

.PHONY: render template
render template:
	helm template $(HELM_RELEASE) . --namespace $(HELM_NAMESPACE) $(HELM_OPTS)

.PHONY: test
test:
	helm test $(HELM_RELEASE) --namespace $(HELM_NAMESPACE) $(HELM_OPTS)

.PHONY: down uninstall
down uninstall:
	helm uninstall $(HELM_RELEASE) --namespace $(HELM_NAMESPACE) $(HELM_OPTS)
