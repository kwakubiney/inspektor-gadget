package networkpolicy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"sort"
	"strings"

	v1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8syaml "sigs.k8s.io/yaml"

	"github.com/kinvolk/inspektor-gadget/pkg/networkpolicy/types"
)

var defaultLabelsToIgnore = []string{
	"controller-revision-hash",
	"pod-template-generation",
	"pod-template-hash",
}

type NetworkPolicyAdvisor struct {
	Events []types.KubernetesConnectionEvent

	LabelsToIgnore []string

	Policies []networkingv1.NetworkPolicy
}

func NewAdvisor() *NetworkPolicyAdvisor {
	return &NetworkPolicyAdvisor{
		LabelsToIgnore: defaultLabelsToIgnore,
	}
}

func (a *NetworkPolicyAdvisor) LoadFile(filename string) error {
	buf, err := ioutil.ReadFile(filename)
	if err != nil {
		return err
	}

	/* Try to read the file as an array */
	events := []types.KubernetesConnectionEvent{}
	err = json.Unmarshal(buf, &events)
	if err == nil {
		a.Events = events
		return nil
	}

	/* If it fails, read by line */
	events = nil
	line := 0
	scanner := bufio.NewScanner(bytes.NewReader(buf))
	for scanner.Scan() {
		event := types.KubernetesConnectionEvent{}
		text := strings.TrimSpace(scanner.Text())
		if len(text) == 0 {
			continue
		}
		line++
		err = json.Unmarshal([]byte(text), &event)
		if err != nil {
			return fmt.Errorf("cannot parse line %d: %s", line, err)
		}
		events = append(events, event)
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	a.Events = events

	return nil
}

/* labelFilteredKeyList returns a sorted list of label keys but without the labels to
 * ignore.
 */
func (a *NetworkPolicyAdvisor) labelFilteredKeyList(labels map[string]string) []string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		found := false
		for _, l := range a.LabelsToIgnore {
			if l == k {
				found = true
				break
			}
		}
		if found {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	return keys
}

func (a *NetworkPolicyAdvisor) labelFilter(labels map[string]string) map[string]string {
	keys := a.labelFilteredKeyList(labels)
	ret := map[string]string{}
	for _, k := range keys {
		ret[k] = labels[k]
	}
	return ret
}

/* labelKeyString returns a sorted list of labels in a single string.
 * label1=value1,label2=value2
 */
func (a *NetworkPolicyAdvisor) labelKeyString(labels map[string]string) (ret string) {
	keys := a.labelFilteredKeyList(labels)

	for index, k := range keys {
		sep := ","
		if index == 0 {
			sep = ""
		}
		ret += fmt.Sprintf("%s%s=%s", sep, k, labels[k])
	}
	return
}

/* localPodKey returns a key that can be used to group pods together:
 * namespace:label1=value1,label2=value2
 */
func (a *NetworkPolicyAdvisor) localPodKey(e types.KubernetesConnectionEvent) (ret string) {
	return e.LocalPodNamespace + ":" + a.labelKeyString(e.LocalPodLabels)
}

func (a *NetworkPolicyAdvisor) networkPeerKey(e types.KubernetesConnectionEvent) (ret string) {
	if e.RemoteKind == "pod" {
		ret = e.RemoteKind + ":" + e.RemotePodNamespace + ":" + a.labelKeyString(e.RemotePodLabels)
	} else if e.RemoteKind == "svc" {
		ret = e.RemoteKind + ":" + e.RemoteSvcNamespace + ":" + a.labelKeyString(e.RemoteSvcLabelSelector)
	} else if e.RemoteKind == "other" {
		ret = e.RemoteKind + ":" + e.RemoteOther
	}
	return ret + ":" + string(e.Port)
}

func (a *NetworkPolicyAdvisor) eventToRule(e types.KubernetesConnectionEvent) (ports []networkingv1.NetworkPolicyPort, peers []networkingv1.NetworkPolicyPeer) {
	port := intstr.FromInt(int(e.Port))
	protocol := v1.Protocol("TCP")
	ports = []networkingv1.NetworkPolicyPort{
		networkingv1.NetworkPolicyPort{
			Port:     &port,
			Protocol: &protocol,
		},
	}
	// TODO: check if LocalPodNamespace != Remote*Namespace
	if e.RemoteKind == "pod" {
		peers = []networkingv1.NetworkPolicyPeer{
			networkingv1.NetworkPolicyPeer{
				PodSelector: &metav1.LabelSelector{MatchLabels: a.labelFilter(e.RemotePodLabels)},
			},
		}
	} else if e.RemoteKind == "svc" {
		peers = []networkingv1.NetworkPolicyPeer{
			networkingv1.NetworkPolicyPeer{
				PodSelector: &metav1.LabelSelector{MatchLabels: e.RemoteSvcLabelSelector},
			},
		}
	} else if e.RemoteKind == "other" {
		peers = []networkingv1.NetworkPolicyPeer{
			networkingv1.NetworkPolicyPeer{
				IPBlock: &networkingv1.IPBlock{
					CIDR: e.RemoteOther + "/32",
				},
			},
		}
	} else {
		panic("unknown event")
	}
	return
}

func (a *NetworkPolicyAdvisor) GeneratePolicies() {
	eventsBySource := map[string][]types.KubernetesConnectionEvent{}
	for _, e := range a.Events {
		key := a.localPodKey(e)
		if _, ok := eventsBySource[key]; ok {
			eventsBySource[key] = append(eventsBySource[key], e)
		} else {
			eventsBySource[key] = []types.KubernetesConnectionEvent{e}
		}
	}

	for _, events := range eventsBySource {
		egressNetworkPeer := map[string][]types.KubernetesConnectionEvent{}
		ingressNetworkPeer := map[string][]types.KubernetesConnectionEvent{}
		for _, e := range events {
			key := a.networkPeerKey(e)
			if e.Type == "connect" {
				if _, ok := egressNetworkPeer[key]; ok {
					egressNetworkPeer[key] = append(egressNetworkPeer[key], e)
				} else {
					egressNetworkPeer[key] = []types.KubernetesConnectionEvent{e}
				}
			} else if e.Type == "accept" {
				if _, ok := ingressNetworkPeer[key]; ok {
					ingressNetworkPeer[key] = append(ingressNetworkPeer[key], e)
				} else {
					ingressNetworkPeer[key] = []types.KubernetesConnectionEvent{e}
				}
			}
		}
		egressPolicies := []networkingv1.NetworkPolicyEgressRule{}
		for _, p := range egressNetworkPeer {
			ports, peers := a.eventToRule(p[0])
			rule := networkingv1.NetworkPolicyEgressRule{
				Ports: ports,
				To:    peers,
			}
			egressPolicies = append(egressPolicies, rule)
		}
		ingressPolicies := []networkingv1.NetworkPolicyIngressRule{}
		for _, p := range ingressNetworkPeer {
			ports, peers := a.eventToRule(p[0])
			rule := networkingv1.NetworkPolicyIngressRule{
				Ports: ports,
				From:  peers,
			}
			ingressPolicies = append(ingressPolicies, rule)
		}

		name := events[0].LocalPodName
		if events[0].LocalPodOwner != "" {
			name = events[0].LocalPodOwner
		}
		name += "-network"
		policy := networkingv1.NetworkPolicy{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "networking.k8s.io/v1",
				Kind:       "NetworkPolicy",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: events[0].LocalPodNamespace,
				Labels:    map[string]string{},
			},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: a.labelFilter(events[0].LocalPodLabels)},
				PolicyTypes: []networkingv1.PolicyType{"Ingress", "Egress"},
				Ingress:     ingressPolicies,
				Egress:      egressPolicies,
			},
		}
		a.Policies = append(a.Policies, policy)
	}

}

func (a *NetworkPolicyAdvisor) PrintPolicies() {
	for i, p := range a.Policies {
		yamlOutput, err := k8syaml.Marshal(p)
		if err != nil {
			continue
		}
		sep := "---\n"
		if i == len(a.Policies)-1 {
			sep = ""
		}
		fmt.Printf("%s%s", string(yamlOutput), sep)
	}
}
