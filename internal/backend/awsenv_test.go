package backend

import (
	"reflect"
	"testing"
)

func TestAWSVars(t *testing.T) {
	if got := (AWS{}).Vars("127.0.0.1"); got != nil {
		t.Errorf("zero AWS Vars() = %v, want nil", got)
	}

	got := AWS{Region: "eu-south-2", Port: 8000}.Vars("127.0.0.1")
	want := map[string]string{
		"AWS_REGION":            "eu-south-2",
		"AWS_DEFAULT_REGION":    "eu-south-2",
		"AWS_ACCESS_KEY_ID":     "lambdary",
		"AWS_SECRET_ACCESS_KEY": "lambdary",
		"AWS_SESSION_TOKEN":     "lambdary",
		"AWS_ENDPOINT_URL":      "http://127.0.0.1:8000",
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Vars() = %v, want %v", got, want)
	}

	if _, ok := (AWS{Region: "r", Port: 8000}).Vars("")["AWS_ENDPOINT_URL"]; ok {
		t.Error("no endpoint host still set an endpoint")
	}

	if _, ok := (AWS{Region: "r"}).Vars("h")["AWS_ENDPOINT_URL"]; ok {
		t.Error("no dev server port still set an endpoint")
	}
}

func TestAWSUnderEnv(t *testing.T) {
	aws := AWS{Region: "us-east-1"}

	got := aws.UnderEnv(map[string]string{"AWS_REGION": "x", "OTHER": "1"}, "")
	if got["AWS_REGION"] != "x" || got["OTHER"] != "1" || got["AWS_ACCESS_KEY_ID"] != "lambdary" {
		t.Errorf("UnderEnv() = %v", got)
	}

	env := map[string]string{"A": "1"}
	if got := (AWS{}).UnderEnv(env, "h"); !reflect.DeepEqual(got, env) {
		t.Errorf("disabled UnderEnv() = %v, want %v", got, env)
	}
}
