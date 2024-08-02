package v1alpha1

import "errors"

func validateOdd(n int32) error {
	if n%2 == 0 {
		return errors.New("Cluster size must be an odd number")
	}
	return nil
}
