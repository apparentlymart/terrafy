# Terrafy Quirks

Terrafy is an experimental tool that exists mainly to explore the problem of
batch importing with configuration generation in Terraform.

It has succeeded in identifying a number of quirky behaviors in
Terraform and its providers that lead to confusing or incorrect behavior
when importing, including the following:

* It seems that the Terraform SDK v2 incorrectly reports the `id` attribute
  for managed resources as being both "optional" and "computed", even though
  setting `id` in the configuration always results in a validation error.

  That's problematic for Terrafy because it thinks it needs to write the `id`
  value into the configuration as an argument, which then causes the resulting
  configuration to fail validation:

  ```
  Error: : invalid or unknown key: id

    on imported.tf line 2, in resource "random_integer" "test":
     2: resource "random_integer" "test" {
  ```

* Because Terraform SDK v2 doesn't have true support for optional arguments
  in the sense of them taking on the value `null`, after importing all optional
  arguments that were not set tend to take on the zero value of their defined
  type, rather than being omitted as expected. For example, with
  `tls_cert_request`'s `subject` block:

  ```hcl
  resource "tls_cert_request" "foo" {
    id              = "fe1324f502e5d92b06c086e31c00e599d7ea5eaf"
    key_algorithm   = "RSA"
    private_key_pem = "087be88c0c388e64bc5464fd6870df6118f80e41"
    subject {
      common_name         = "foo"
      country             = ""
      locality            = ""
      organization        = ""
      organizational_unit = ""
      postal_code         = ""
      province            = ""
      serial_number       = ""
    }
  }
  ```

* Also visible in the previous example: SDKv2 has an odd feature where the
  provider is allowed to override the true value of an argument to a different
  value in the state. The Terraform provider protocol forbids this but has
  an exception for the "legacy SDK" (which SDKv2 is still considered to be)
  to give provider teams some time to remove uses of this feature.

  However, we can see in the above example that it's still in use for the
  `private_key_pem` argument of `tls_cert_request`, causing Terrafy to see
  the value as a hash of the private key instead of the private key itself.

* When an argument is marked as sensitive in the provider schema, it's not
  clear what is a reasonable behavior to take under import. It certainly
  isn't acceptable to write that secret value in cleartext into the config,
  but that's currently what Terrafy does in the absense of any better idea.

* Because Terrafy is generating configuration mechanically from the state, it
  can't tell if a particular argument ought to be explicitly set or whether
  it would be better to leave it unset and let a computed default be used
  instead. Currently it errs on the side of including all arguments that are
  set in the state, which will generate more configuration than strictly
  necessary in lots of cases.

  This could potentially be improved by introducing an optional new provider
  protocol operation that takes an existing state value as an argument, where
  the provider's job is to return another value conforming to the resource
  type schema that reflects what could be written in the configuration to
  achieve that result. Then the SDK or the provider logic itself could use
  its knowledge about the remote system to understand when a particular value
  is what the remote system would've chosen by default, and leave the
  configuration argument set to `null` in that case so Terrafy would omit it.

* When importing several existing objects into a single resource with `count`
  or `for_each`, Terrafy has no general way to infer what systematic process
  (if any) produced the differences between those objects in order to reflect
  it as expressions in the configuration.

  Currently Terrafy generates a lookup table against either `count.index` or
  `each.key` in that case, verbosely writing out the value associated with
  each index/key in a tuple or object type constructor. For example, `seed`
  in the following example was generated in that way:

  ```hcl
  resource "random_integer" "test" {
    count = 3

    id  = "1"
    max = 1000
    min = 0
    seed = [
      "foo",
      "bar",
      "baz",
    ][count.index]
  }
  ```

  This achieves the necessary result, but is not in any way idiomatic and would
  likely be confusing to a new Terraform user that is unfamiliar with more
  usual patterns in the Terraform language.

* Nested blocks create a difficult situation when different objects that are
  represented as instances of the same resource differ in the number of blocks.
  Terrafy must generate `dynamic` blocks in that case, with conditional
  expressions to determine whether to generate a block in each case.

  This isn't currently implemented, and so Terrafy will fail to produce a
  correct configuration when encountering that situation.

* In order to generate a correct configuration, Terrafy should be able to
  write out either implicit or explicit dependencies between resources so that
  Terraform can understand their dependency relationships. If not, incorrect
  behavior is likely to result later on for commands like `terraform destroy`,
  which uses dependency relationships to understand the correct destroy order.

  Terrafy doesn't currently do anything about this. In principle it could
  try to detect situations where the value of one resource argument matches
  the value of an "identifying" attribute of another resource (`id`, `name`,
  `arn`, etc) but that's complicated again by the fact that a resource can
  potentially have multiple instances that disagree about their argument values.
