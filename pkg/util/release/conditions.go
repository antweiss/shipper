package release

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	shipper "github.com/bookingcom/shipper/pkg/apis/shipper/v1alpha1"
	diffutil "github.com/bookingcom/shipper/pkg/util/diff"
)

var ConditionsShouldDiscardTimestamps = false

type ReleaseConditionDiff struct {
	c1, c2 *shipper.ReleaseCondition
}

var _ diffutil.Diff = (*ReleaseConditionDiff)(nil)

func NewReleaseConditionDiff(c1, c2 *shipper.ReleaseCondition) *ReleaseConditionDiff {
	return &ReleaseConditionDiff{
		c1: c1,
		c2: c2,
	}
}

func (d *ReleaseConditionDiff) IsEmpty() bool {
	if d.c1 == nil && d.c2 == nil {
		return true
	}
	if d.c1 == nil || d.c2 == nil {
		return false
	}
	return d.c1.Type == d.c2.Type &&
		d.c1.Status == d.c2.Status &&
		d.c1.Reason == d.c2.Reason &&
		d.c1.Message == d.c2.Message
}

func (d *ReleaseConditionDiff) String() string {
	if d.IsEmpty() {
		return ""
	}
	c1str, c2str := condStr(d.c1), condStr(d.c2)
	return fmt.Sprintf("[%s] -> [%s]", c1str, c2str)
}

func NewReleaseCondition(condType shipper.ReleaseConditionType, status corev1.ConditionStatus, reason, message string) *shipper.ReleaseCondition {
	now := metav1.Now()
	if ConditionsShouldDiscardTimestamps {
		now = metav1.Time{}
	}
	return &shipper.ReleaseCondition{
		Type:               condType,
		Status:             status,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
	}
}

func SetReleaseCondition(status *shipper.ReleaseStatus, condition shipper.ReleaseCondition) diffutil.Diff {
	currentCond := GetReleaseCondition(*status, condition.Type)

	diff := NewReleaseConditionDiff(currentCond, &condition)
	if diff.IsEmpty() {
		return nil
	}

	if currentCond != nil && currentCond.Status == condition.Status {
		condition.LastTransitionTime = currentCond.LastTransitionTime
	}
	newConditions := filterOutCondition(status.Conditions, condition.Type)
	status.Conditions = append(newConditions, condition)
	sort.Slice(status.Conditions, func(i, j int) bool {
		return status.Conditions[i].Type < status.Conditions[j].Type
	})

	return diff
}

func GetReleaseCondition(status shipper.ReleaseStatus, condType shipper.ReleaseConditionType) *shipper.ReleaseCondition {
	for i := range status.Conditions {
		c := status.Conditions[i]
		if c.Type == condType {
			return &c
		}
	}
	return nil
}

func RemoveReleaseCondition(status shipper.ReleaseStatus, condType shipper.ReleaseConditionType) {
	status.Conditions = filterOutCondition(status.Conditions, condType)
}

func ReleaseScheduled(release *shipper.Release) bool {
	scheduledCond := GetReleaseCondition(release.Status, shipper.ReleaseConditionTypeScheduled)
	return scheduledCond != nil && scheduledCond.Status == corev1.ConditionTrue
}

func ReleaseComplete(release *shipper.Release) bool {
	releasedCond := GetReleaseCondition(release.Status, shipper.ReleaseConditionTypeComplete)
	return releasedCond != nil && releasedCond.Status == corev1.ConditionTrue
}

func ReleaseProgressing(release *shipper.Release) bool {
	return !(ReleaseComplete(release))
}

func filterOutCondition(conditions []shipper.ReleaseCondition, condType shipper.ReleaseConditionType) []shipper.ReleaseCondition {
	var newConditions []shipper.ReleaseCondition
	for _, c := range conditions {
		if c.Type == condType {
			continue
		}
		newConditions = append(newConditions, c)
	}
	return newConditions
}

func condStr(c *shipper.ReleaseCondition) string {
	if c == nil {
		return ""
	}
	chunks := []string{
		fmt.Sprintf("%v", c.Type),
		fmt.Sprintf("%v", c.Status),
		c.Reason,
		c.Message,
	}
	b := strings.Builder{}
	for _, ch := range chunks {
		if len(ch) > 0 {
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(ch)
		}
	}
	return b.String()
}
