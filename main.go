package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var (
	runtimeScheme = runtime.NewScheme()
	codecs        = serializer.NewCodecFactory(runtimeScheme)
	deserializer  = codecs.UniversalDeserializer()
)

const (
	spotTolerationKey    = "kubernetes.azure.com/scalesetpriority"
	spotTolerationValue  = "spot"
	spotTolerationEffect = corev1.TaintEffectNoSchedule
)

var ignoredNamespaces = []string{
	metav1.NamespaceSystem,
	"cert-manager",
	"gatekeeper-system",
}

type PodMutator struct {
	Client  http.Client
	decoder *admission.Decoder
}

func (m *PodMutator) Handle(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		if data, err := io.ReadAll(r.Body); err == nil {
			body = data
		}
	}
	if len(body) == 0 {
		log.Log.Error(nil, "empty body")
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}

	ar := v1beta1.AdmissionReview{}
	if _, _, err := deserializer.Decode(body, nil, &ar); err != nil {
		log.Log.Error(err, "cant decode body")
		http.Error(w, "can't decode body", http.StatusBadRequest)
		return
	}

	pod := corev1.Pod{}
	if err := json.Unmarshal(ar.Request.Object.Raw, &pod); err != nil {
		log.Log.Error(err, "unable to unmarshal pod")
		http.Error(w, "unable to unmarshal pod", http.StatusBadRequest)
		return
	}

	admissionResponse := &v1beta1.AdmissionResponse{
		Allowed: true,
		UID:     ar.Request.UID,
	}

	for _, namespace := range ignoredNamespaces {
		if pod.Namespace == namespace {
			log.Log.Info("Skipping pod in ignored namespace", "namespace", pod.Namespace, "pod", pod.Name)
			w.Header().Set("Content-Type", "application/json")
			js, err := json.Marshal(v1beta1.AdmissionReview{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "admission.k8s.io/v1",
					Kind:       "AdmissionReview",
				},
				Response: admissionResponse,
			})
			if err != nil {
				http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			w.Write(js)
			return
		}
	}

	hasToleration := false
	for _, toleration := range pod.Spec.Tolerations {
		if toleration.Key == spotTolerationKey && toleration.Value == spotTolerationValue && toleration.Effect == spotTolerationEffect {
			hasToleration = true
			break
		}
	}

	if !hasToleration {
		log.Log.Info("Injecting spot toleration", "pod", pod.Name)
		toleration := corev1.Toleration{
			Key:      spotTolerationKey,
			Value:    spotTolerationValue,
			Effect:   spotTolerationEffect,
			Operator: corev1.TolerationOpEqual,
		}
		pod.Spec.Tolerations = append(pod.Spec.Tolerations, toleration)

		patch := []struct {
			Op    string      `json:"op"`
			Path  string      `json:"path"`
			Value interface{} `json:"value"`
		}{{
			Op:    "replace",
			Path:  "/spec/tolerations",
			Value: pod.Spec.Tolerations,
		}}

		patchBytes, err := json.Marshal(patch)
		if err != nil {
			log.Log.Error(err, "unable to marshal patch")
			http.Error(w, "unable to marshal patch", http.StatusInternalServerError)
			return
		}

		admissionResponse.Patch = patchBytes
		patchType := v1beta1.PatchTypeJSONPatch
		admissionResponse.PatchType = &patchType
	} else {
		log.Log.Info("Pod already has spot toleration", "pod", pod.Name)
	}

	finalAdmissionReview := v1beta1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Response: admissionResponse,
	}

	w.Header().Set("Content-Type", "application/json")
	js, err := json.Marshal(finalAdmissionReview)
	if err != nil {
		log.Log.Error(err, "could not encode response")
		http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write(js)
}

func main() {
	log.SetLogger(zap.New())
	log.Log.Info("Starting webhook server")

	mux := http.NewServeMux()
	mux.HandleFunc("/mutate", (&PodMutator{}).Handle)

	server := &http.Server{
		Addr:    ":8443",
		Handler: mux,
	}

	log.Log.Info("Listening on port 8443")
	if err := server.ListenAndServeTLS("/tmp/k8s-webhook-server/serving-certs/tls.crt", "/tmp/k8s-webhook-server/serving-certs/tls.key"); err != nil {
		log.Log.Error(err, "failed to start server")
	}
}
