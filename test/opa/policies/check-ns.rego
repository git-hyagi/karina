package kubernetes.admission  
  
import data.kubernetes.namespaces
import data.automobile.companies

has_key(obj, k) { _ = obj[k]}
has_item(list, i) {list[_] = i}

deny[msg] {
    input.request.kind.kind = "Namespace"
    input.request.operation = "CREATE"
    metadata = input.request.object.metadata
    not has_key(metadata,"labels")
    msg = "Must specify label: company and service"
}

deny[msg] {
    input.request.kind.kind = "Namespace"
    input.request.operation = "CREATE"
    incoming_service = input.request.object.metadata.labels.service
    incoming_company = input.request.object.metadata.labels.company
    labels = input.request.object.metadata.labels
    not has_key(companies,incoming_company )
    msg = sprintf("Company name %q does not exist",[incoming_company])
}

deny[msg] {
    input.request.kind.kind = "Namespace"
    input.request.operation = "CREATE"
    incoming_service = input.request.object.metadata.labels.service
    incoming_company = input.request.object.metadata.labels.company
    services = companies[incoming_company]
    labels = input.request.object.metadata.labels
    not has_item(services, incoming_service)
    msg = sprintf("Service %q not found for company %q",[incoming_service,incoming_company])
}