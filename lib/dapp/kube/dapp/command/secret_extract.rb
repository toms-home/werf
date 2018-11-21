module Dapp
  module Kube
    module Dapp
      module Command
        module SecretExtract
          def kube_secret_extract(file_path)
            ruby2go_deploy_command(command: :secret_extract, command_options: kube_secret_extract_command_options(file_path))
          end

          def kube_secret_extract_command_options(file_path)
            kube_secret_generate_command_options(file_path)
          end
        end
      end
    end
  end
end
